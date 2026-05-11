package raft

// MVCCStateMachine implements multi-version concurrency control for the KV
// store (ADR-022). It maintains a version history per key and supports
// compare-and-swap (CAS) writes and snapshot reads.
//
// Invariants:
//   - INV-MV1: latest[key] always equals versions[key][last].Version.
//   - INV-MV2: Version numbers are strictly monotonic per key across restarts.
//   - INV-MV3: A CAS conflict does NOT advance lastApplied; notifyWaiters is
//     still called so HTTP handlers are not blocked.
//   - INV-MV4: Unconditional writes (expected_version==0) never conflict.
//   - INV-MV5: Snapshot reads (?version=N) bypass ReadIndex; they are not
//     linearizable but are repeatable.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
)

// ErrSnapshotDisplaced is returned by WaitForIndex when a snapshot install
// completed before the awaited entry was individually applied. The HTTP handler
// must retry rather than interpret the result as a success or conflict.
var ErrSnapshotDisplaced = errors.New("mvcc: entry displaced by snapshot install; retry")

// MVCCVersion is a single immutable snapshot of a key's value.
// Version is monotonically increasing per key (INV-MV1, INV-MV2).
type MVCCVersion struct {
	Version uint64 // 1-based; 0 is never stored
	Value   string
	Deleted bool
}

// mvccSnapshotPayload is the JSON-serialisable representation of the full
// MVCCStateMachine state, embedded in SnapshotData.KV under a reserved key.
type mvccSnapshotPayload struct {
	Versions map[string][]MVCCVersion `json:"versions"`
	Latest   map[string]uint64        `json:"latest"`
}

// snapshotMVCCKey is a reserved key used to embed MVCC state in SnapshotData.
// The NUL prefix prevents collision with user KV keys.
const snapshotMVCCKey = "\x00__mvcc_state__"

// MVCCStateMachine is a Raft state machine that maintains version history per
// key. It is a drop-in replacement for KVStateMachine where MVCC semantics are
// required (ADR-022).
//
// Thread safety:
//   - mu protects versions, latest, conflictResults, and lastApplied.
//     lastApplied is additionally exposed via atomic load for WaitForIndex
//     fast-path; it is stored only while mu is held (write lock) so that
//     TakeSnapshot sees a consistent (versions, lastApplied) pair.
//   - waitMu protects waiters.
type MVCCStateMachine struct {
	mu       sync.RWMutex
	versions map[string][]MVCCVersion // key → versions, oldest first
	latest   map[string]uint64        // key → current version number (INV-MV1)

	// conflictResults maps Raft log index → version at conflict time (INV-MV3).
	// Presence indicates a CAS conflict. Value is the version that was current
	// when the conflict was detected, embedded in the 409 response body.
	// Entries are deleted by IsCASConflict after being read (leak prevention).
	conflictResults map[int64]uint64

	// retention is the maximum number of versions to keep per key.
	// 0 means keep all (unbounded; for tests or explicit opt-in).
	retention int

	waitMu      sync.Mutex
	waiters     map[int64][]chan error // buffered(1); nil=applied, non-nil=error
	lastApplied atomic.Int64
}

// NewMVCCStateMachine creates an MVCCStateMachine.
//
// store is accepted for interface compatibility with the startup wiring but is
// unused: MVCCStateMachine is in-memory only (ADR-022 scope).
//
// retention controls how many versions to keep per key. 0 keeps all.
func NewMVCCStateMachine(_ KVDurableStore, retention int) *MVCCStateMachine {
	return &MVCCStateMachine{
		versions:        make(map[string][]MVCCVersion),
		latest:          make(map[string]uint64),
		conflictResults: make(map[int64]uint64),
		waiters:         make(map[int64][]chan error),
		retention:       retention,
	}
}

// Run consumes entries from applyCh until ctx is cancelled.
// Call this in a dedicated goroutine.
func (sm *MVCCStateMachine) Run(ctx context.Context, applyCh <-chan LogEntry) {
	for {
		select {
		case <-ctx.Done():
			return
		case entry, ok := <-applyCh:
			if !ok {
				return
			}
			sm.apply(entry)
		}
	}
}

// apply processes a single committed log entry.
//
// Apply ordering for successful writes:
//  1. Parse command.
//  2. CAS check (if ExpectedVersion > 0): on mismatch, record conflict version
//     and notify waiters WITHOUT advancing lastApplied (INV-MV3).
//  3. Write new version, advance latest, GC old versions.
//  4. lastApplied.Store(entry.Index) — inside mu to keep TakeSnapshot consistent.
//  5. notifyWaiters(entry.Index, nil).
func (sm *MVCCStateMachine) apply(entry LogEntry) {
	var cmd RaftKVCommand
	if err := json.Unmarshal(entry.Data, &cmd); err != nil {
		slog.Warn("mvcc: ignored malformed entry", "index", entry.Index, "err", err)
		sm.notifyWaiters(entry.Index, nil)
		return
	}

	sm.mu.Lock()

	// INV-MV4: unconditional writes (expected_version==0) never conflict.
	// INV-MV3: CAS conflict does NOT advance lastApplied.
	if cmd.ExpectedVersion > 0 {
		current := sm.latest[cmd.Key] // 0 if key does not exist
		if current != cmd.ExpectedVersion {
			// Store the version at conflict time for the 409 response body (FIX-3).
			sm.conflictResults[entry.Index] = current
			sm.mu.Unlock()
			sm.notifyWaiters(entry.Index, nil)
			slog.Debug("mvcc: CAS conflict",
				"key", cmd.Key, "expected", cmd.ExpectedVersion,
				"current", current, "index", entry.Index)
			return
		}
	}

	nextVer := sm.latest[cmd.Key] + 1 // INV-MV2: strictly monotonic
	deleted := cmd.Op == "del"
	sm.versions[cmd.Key] = append(sm.versions[cmd.Key], MVCCVersion{
		Version: nextVer,
		Value:   cmd.Value,
		Deleted: deleted,
	})
	sm.latest[cmd.Key] = nextVer // INV-MV1 maintained

	sm.gc(cmd.Key)

	// FIX-5: store lastApplied inside mu so TakeSnapshot (mu.RLock) always
	// observes a (versions, lastApplied) pair that corresponds to the same
	// logical state. If Store were outside the lock, TakeSnapshot could capture
	// versions at index N but lastApplied at N-1.
	sm.lastApplied.Store(entry.Index)

	sm.mu.Unlock()

	slog.Debug("mvcc: applied", "op", cmd.Op, "key", cmd.Key,
		"version", nextVer, "index", entry.Index)

	sm.notifyWaiters(entry.Index, nil)
}

// gc trims versions[key] so that at most sm.retention versions are kept.
// Must be called with sm.mu held (write lock). retention==0 disables GC.
func (sm *MVCCStateMachine) gc(key string) {
	if sm.retention == 0 {
		return
	}
	vs := sm.versions[key]
	if len(vs) > sm.retention {
		sm.versions[key] = vs[len(vs)-sm.retention:]
	}
}

// LastApplied returns the index of the most recently applied log entry.
func (sm *MVCCStateMachine) LastApplied() int64 {
	return sm.lastApplied.Load()
}

// WaitForIndex blocks until the entry at index has been applied (or a CAS
// conflict recorded for it), or ctx expires, or a snapshot displaces the entry.
//
// Return values:
//   - nil: applied or CAS conflict recorded — caller must check IsCASConflict.
//   - ErrSnapshotDisplaced: snapshot installed before individual apply; retry.
//   - ctx.Err(): deadline exceeded.
func (sm *MVCCStateMachine) WaitForIndex(ctx context.Context, index int64) error {
	// Fast-path 1: already applied.
	if sm.lastApplied.Load() >= index {
		return nil
	}
	// Fast-path 2: CAS conflict already recorded for this index.
	sm.mu.RLock()
	_, hasConflict := sm.conflictResults[index]
	sm.mu.RUnlock()
	if hasConflict {
		return nil
	}

	ch := make(chan error, 1) // buffered so notifyWaiters never blocks

	sm.waitMu.Lock()
	sm.waiters[index] = append(sm.waiters[index], ch)
	sm.waitMu.Unlock()

	select {
	case err := <-ch:
		return err // nil on success/conflict; ErrSnapshotDisplaced on snapshot
	case <-ctx.Done():
		sm.waitMu.Lock()
		waiters := sm.waiters[index]
		filtered := waiters[:0]
		for _, w := range waiters {
			if w != ch {
				filtered = append(filtered, w)
			}
		}
		if len(filtered) == 0 {
			delete(sm.waiters, index)
		} else {
			sm.waiters[index] = filtered
		}
		sm.waitMu.Unlock()
		return ctx.Err()
	}
}

// IsCASConflict reports whether the entry at index was a CAS conflict and
// returns the version that was current at the time of the conflict.
//
// This call is destructive: the entry is removed from conflictResults to
// prevent unbounded memory growth (FIX-4).
// Must be called after WaitForIndex returns nil for the same index.
func (sm *MVCCStateMachine) IsCASConflict(index int64) (currentVersion uint64, conflict bool) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	ver, ok := sm.conflictResults[index]
	if ok {
		delete(sm.conflictResults, index)
	}
	return ver, ok
}

// Get returns the latest value for key and whether it exists (not deleted).
func (sm *MVCCStateMachine) Get(key string) (value string, version uint64, found bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	ver, ok := sm.latest[key]
	if !ok {
		return "", 0, false
	}
	vs := sm.versions[key]
	if len(vs) == 0 {
		return "", 0, false
	}
	last := vs[len(vs)-1] // INV-MV1: last entry is always the latest version
	if last.Deleted {
		return "", ver, false
	}
	return last.Value, ver, true
}

// GetVersion returns the value at the specific version for key (snapshot read).
//
// INV-MV5: snapshot reads bypass ReadIndex; not linearizable but repeatable.
// Returns ("", false) when the version is not found (GC'd or never written).
func (sm *MVCCStateMachine) GetVersion(key string, version uint64) (value string, found bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	for _, v := range sm.versions[key] {
		if v.Version == version {
			if v.Deleted {
				return "", false
			}
			return v.Value, true
		}
	}
	return "", false
}

// TakeSnapshot captures the full MVCC state as a SnapshotData.
//
// Consistency guarantee (FIX-5): sm.mu.RLock() is held for the full duration.
// Because apply() stores lastApplied inside mu, the captured (versions,
// lastApplied) pair is always consistent — no half-applied entry can appear.
func (sm *MVCCStateMachine) TakeSnapshot() (SnapshotData, int64, error) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	capturedIndex := sm.lastApplied.Load() // safe: Store is also under mu

	vCopy := make(map[string][]MVCCVersion, len(sm.versions))
	for k, vs := range sm.versions {
		c := make([]MVCCVersion, len(vs))
		copy(c, vs)
		vCopy[k] = c
	}
	lCopy := make(map[string]uint64, len(sm.latest))
	for k, v := range sm.latest {
		lCopy[k] = v
	}

	payload := mvccSnapshotPayload{Versions: vCopy, Latest: lCopy}
	b, err := json.Marshal(payload)
	if err != nil {
		return SnapshotData{}, 0, err
	}

	kv := map[string]string{snapshotMVCCKey: string(b)}
	return SnapshotData{KV: kv}, capturedIndex, nil
}

// RestoreSnapshot replaces the full MVCC state from a SnapshotData.
//
// All in-flight WaitForIndex waiters for indices <= lastApplied are unblocked
// with ErrSnapshotDisplaced (FIX-1): their individual apply() calls will never
// come, so HTTP handlers must retry rather than treat the result as success.
func (sm *MVCCStateMachine) RestoreSnapshot(data SnapshotData, lastApplied int64) error {
	raw, ok := data.KV[snapshotMVCCKey]
	if !ok {
		sm.mu.Lock()
		sm.versions = make(map[string][]MVCCVersion)
		sm.latest = make(map[string]uint64)
		sm.conflictResults = make(map[int64]uint64)
		sm.lastApplied.Store(lastApplied)
		sm.mu.Unlock()
		sm.displaceWaitersUpTo(lastApplied)
		return nil
	}

	var payload mvccSnapshotPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return err
	}

	sm.mu.Lock()
	sm.versions = payload.Versions
	sm.latest = payload.Latest
	sm.conflictResults = make(map[int64]uint64)
	sm.lastApplied.Store(lastApplied)
	sm.mu.Unlock()

	sm.displaceWaitersUpTo(lastApplied)
	return nil
}

// notifyWaiters unblocks all waiters registered for index, delivering err.
// err==nil signals a normal apply or CAS conflict (caller checks IsCASConflict).
// err==ErrSnapshotDisplaced signals the snapshot displacement path.
func (sm *MVCCStateMachine) notifyWaiters(index int64, err error) {
	sm.waitMu.Lock()
	waiters := sm.waiters[index]
	delete(sm.waiters, index)
	sm.waitMu.Unlock()

	for _, ch := range waiters {
		ch <- err // buffered(1): never blocks
	}
}

// displaceWaitersUpTo sends ErrSnapshotDisplaced to every waiter registered
// for any index <= upTo. Called by RestoreSnapshot (FIX-1).
func (sm *MVCCStateMachine) displaceWaitersUpTo(upTo int64) {
	sm.waitMu.Lock()
	var toNotify []chan error
	for idx, chans := range sm.waiters {
		if idx <= upTo {
			toNotify = append(toNotify, chans...)
			delete(sm.waiters, idx)
		}
	}
	sm.waitMu.Unlock()

	for _, ch := range toNotify {
		ch <- ErrSnapshotDisplaced
	}
}
