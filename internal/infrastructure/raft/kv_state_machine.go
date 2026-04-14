package raft

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
)

// RaftKVCommand is the payload stored in LogEntry.Data for Raft KV operations.
type RaftKVCommand struct {
	Op    string `json:"op"`    // "set" | "del"
	Key   string `json:"key"`
	Value string `json:"value"` // empty for "del"
}

// KVDurableStore is the persistence interface required by KVStateMachine.
//
// Abstracted as an interface so that:
//   - Production uses kv.Store (Bitcask WAL-backed).
//   - Tests use MemKVDurableStore (in-memory, no I/O).
//
// Invariant (INV-1, ADR-016): WriteKV/DeleteKV must complete before the
// caller advances lastApplied. This interface does not enforce ordering; the
// apply() method in KVStateMachine does.
type KVDurableStore interface {
	// WriteKV durably persists key=value at the given Raft log index.
	// Returns ErrReservedKey if key is an internal reserved key.
	WriteKV(key, value string, raftIndex int64) error

	// DeleteKV durably records a delete tombstone for key at raftIndex.
	DeleteKV(key string, raftIndex int64) error

	// GetKV retrieves the current value for key.
	// Returns ("", false, nil) when the key does not exist.
	GetKV(key string) (string, bool, error)

	// RecoverKV replays the durable store and returns the last applied index.
	// Called once at startup; the caller must not issue writes concurrently.
	RecoverKV() (lastAppliedIndex int64, err error)
}

// KVStateMachine consumes committed LogEntry values from ApplyCh and maintains
// an in-memory key-value store. It also supports synchronous apply-wait via
// WaitForIndex, which lets HTTP handlers block until a specific log index has
// been applied before returning a response.
//
// Phase 8: when store != nil, each apply() call durably writes to Bitcask
// before advancing lastApplied and notifying waiters. This enforces INV-1.
type KVStateMachine struct {
	mu   sync.RWMutex
	data map[string]string // in-memory read cache (maintained alongside durable store)

	waitMu  sync.Mutex
	waiters map[int64][]chan struct{}

	// store is the durable persistence backend. nil means in-memory only (test mode).
	store KVDurableStore

	// lastApplied is the Raft log index of the most recently applied entry.
	// Advanced atomically only after a successful durable write (or after
	// in-memory update when store == nil).
	lastApplied atomic.Int64
}

// NewKVStateMachine creates a KVStateMachine.
//
// store may be nil for in-memory-only mode (used in tests and single-node
// scenarios without durability). When nil, apply() skips Bitcask writes and
// behaves identically to the Phase 6/7 implementation.
func NewKVStateMachine(store KVDurableStore) *KVStateMachine {
	return &KVStateMachine{
		data:    make(map[string]string),
		waiters: make(map[int64][]chan struct{}),
		store:   store,
	}
}

// Run consumes entries from applyCh until ctx is cancelled.
// Call this in a dedicated goroutine after raftNode.Run().
func (sm *KVStateMachine) Run(ctx context.Context, applyCh <-chan LogEntry) {
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
// Write ordering (INV-1, ADR-016 §쓰기 순서):
//
//  1. Durable write (store.WriteKV / store.DeleteKV) — if store != nil
//  2. In-memory cache update (sm.data)
//  3. lastApplied.Store(entry.Index)
//  4. notifyWaiters(entry.Index)
//
// If the durable write fails, steps 2–4 are skipped. lastApplied does not
// advance, so the entry can be re-applied after restart (Raft log replay).
// HTTP handlers waiting on this index will time out and receive an error.
func (sm *KVStateMachine) apply(entry LogEntry) {
	var cmd RaftKVCommand
	if err := json.Unmarshal(entry.Data, &cmd); err != nil {
		slog.Warn("raft: kv state machine ignored malformed entry",
			"index", entry.Index, "err", err)
		// Malformed entry: notify waiters so HTTP handlers are not blocked
		// forever. lastApplied is NOT advanced — the entry is considered
		// skipped, not applied.
		sm.notifyWaiters(entry.Index)
		return
	}

	// Step 1: Durable write (Bitcask WAL).
	if sm.store != nil {
		var storeErr error
		switch cmd.Op {
		case "set":
			storeErr = sm.store.WriteKV(cmd.Key, cmd.Value, entry.Index)
		case "del":
			storeErr = sm.store.DeleteKV(cmd.Key, entry.Index)
		}
		if storeErr != nil {
			// Durable write failed (e.g. ENOSPC). lastApplied must NOT advance.
			// notifyWaiters is intentionally omitted: the HTTP handler's
			// WaitForIndex will time out, signalling an error to the client.
			// Raft heartbeat and election continue unaffected.
			slog.Error("raft: kv durability write failed — lastApplied not advanced",
				"index", entry.Index, "op", cmd.Op, "key", cmd.Key, "err", storeErr)
			return
		}
	}

	// Step 2: In-memory cache update.
	sm.mu.Lock()
	switch cmd.Op {
	case "set":
		sm.data[cmd.Key] = cmd.Value
	case "del":
		delete(sm.data, cmd.Key)
	default:
		slog.Warn("raft: kv state machine unknown op", "op", cmd.Op, "index", entry.Index)
	}
	sm.mu.Unlock()

	// Step 3: Advance lastApplied (Bitcask write already succeeded).
	sm.lastApplied.Store(entry.Index)

	slog.Debug("raft: kv applied", "op", cmd.Op, "key", cmd.Key, "index", entry.Index)

	// Step 4: Unblock HTTP handlers waiting on this index.
	sm.notifyWaiters(entry.Index)
}

// ApplyDirect applies a log entry directly without going through the applyCh.
//
// Used during startup recovery to replay entries from the Raft log that are
// not yet reflected in the Bitcask store. Must be called before Run() to
// avoid concurrent access.
//
// INV-3 enforcement: if entry.Index <= lastApplied, this is a no-op (the
// entry has already been applied; re-applying would be a duplicate).
func (sm *KVStateMachine) ApplyDirect(entry LogEntry) {
	if entry.Index <= sm.lastApplied.Load() {
		return // INV-3: no duplicate apply
	}
	sm.apply(entry)
}

// RecoverFromStore replays the durable store to rebuild the in-memory cache
// and returns the last Raft log index that was durably applied.
//
// Procedure:
//  1. sm.store.RecoverKV() → replay WAL, rebuild index, return bitcaskLastApplied
//  2. Populate sm.data from all KV entries visible in the store's index
//  3. Set sm.lastApplied = bitcaskLastApplied
//
// The caller (cmd/main.go) compares the returned index against the Raft log's
// last index and re-applies any gap via ApplyDirect.
//
// Returns (0, nil) when store == nil (in-memory mode, no recovery needed).
func (sm *KVStateMachine) RecoverFromStore(entries []LogEntry) (int64, error) {
	if sm.store == nil {
		return 0, nil
	}

	bitcaskLastApplied, err := sm.store.RecoverKV()
	if err != nil {
		return 0, err
	}

	// Populate in-memory cache from all keys in the recovered store index.
	// We do this by iterating over entries that are <= bitcaskLastApplied and
	// looking them up in the store. This avoids needing a "AllKeys" API on
	// KVDurableStore: we trust the WAL replay to have rebuilt the store index,
	// and we rebuild sm.data by re-reading each applied entry's key/value.
	//
	// Alternative: add AllKeys() to KVDurableStore. Deferred to Phase 9 (LRU cache).
	// For now, we re-apply entries from the Raft log that are already in Bitcask
	// to populate sm.data WITHOUT writing to the store again.
	sm.mu.Lock()
	for _, e := range entries {
		if e.Index > bitcaskLastApplied {
			break
		}
		var cmd RaftKVCommand
		if err := json.Unmarshal(e.Data, &cmd); err != nil {
			continue // malformed entry — skip (consistent with apply behaviour)
		}
		switch cmd.Op {
		case "set":
			sm.data[cmd.Key] = cmd.Value
		case "del":
			delete(sm.data, cmd.Key)
		}
	}
	sm.mu.Unlock()

	sm.lastApplied.Store(bitcaskLastApplied)
	return bitcaskLastApplied, nil
}

// LastApplied returns the index of the most recently applied log entry.
func (sm *KVStateMachine) LastApplied() int64 {
	return sm.lastApplied.Load()
}

// Get returns the value for key and whether it exists.
func (sm *KVStateMachine) Get(key string) (string, bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	v, ok := sm.data[key]
	return v, ok
}

// WaitForIndex blocks until the entry at index has been applied, or ctx
// expires. Used by HTTP handlers for linearizable reads-after-writes.
func (sm *KVStateMachine) WaitForIndex(ctx context.Context, index int64) error {
	// Fast-path: already applied.
	if sm.lastApplied.Load() >= index {
		return nil
	}

	ch := make(chan struct{}, 1)

	sm.waitMu.Lock()
	sm.waiters[index] = append(sm.waiters[index], ch)
	sm.waitMu.Unlock()

	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		// Clean up the registered waiter to prevent a goroutine leak.
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

// TakeSnapshot implements Snapshotable.
//
// COW correctness: sm.mu.RLock() is held for the entire duration so that
// sm.data and sm.lastApplied are captured as a consistent pair. Any
// concurrent apply() call blocks until RUnlock(). Mutations after RUnlock
// cannot affect the returned SnapshotData because snapshot is a fresh map.
func (sm *KVStateMachine) TakeSnapshot() (SnapshotData, int64, error) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	// Capture lastApplied and data under the same read lock (INV-S1 consistency).
	capturedIndex := sm.lastApplied.Load()
	snapshot := make(map[string]string, len(sm.data))
	for k, v := range sm.data {
		snapshot[k] = v
	}
	return SnapshotData{KV: snapshot}, capturedIndex, nil
}

// RestoreSnapshot implements Snapshotable.
//
// Replaces sm.data atomically under a write lock and updates lastApplied.
// Called either during startup (before Run()) or by InstallSnapshot (Phase 9b).
// Concurrent apply() calls are excluded by sm.mu.Lock().
func (sm *KVStateMachine) RestoreSnapshot(data SnapshotData, lastApplied int64) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	sm.data = make(map[string]string, len(data.KV))
	for k, v := range data.KV {
		sm.data[k] = v
	}
	sm.lastApplied.Store(lastApplied)
	return nil
}

func (sm *KVStateMachine) notifyWaiters(index int64) {
	sm.waitMu.Lock()
	waiters := sm.waiters[index]
	delete(sm.waiters, index)
	sm.waitMu.Unlock()

	for _, ch := range waiters {
		close(ch)
	}
}
