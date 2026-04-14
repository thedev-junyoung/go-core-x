package raft

import (
	"context"
	"log/slog"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/junyoung/core-x/proto/pb"
)

// LogEntry is the in-memory representation of a Raft log record.
// The on-wire format is pb.LogEntry; this avoids a proto heap allocation
// on the hot read path inside HandleAppendEntries.
type LogEntry struct {
	Index int64
	Term  int64
	Data  []byte
}

// RaftNode implements the Raft consensus state machine.
// It manages transitions between Follower, Candidate, and Leader roles.
//
// Thread safety: HandleAppendEntries and HandleRequestVote are called from
// gRPC goroutines. The run loop runs in a separate goroutine. Access to
// mutable state is protected by mu.
type RaftNode struct {
	id    string
	peers *PeerClients // nil in tests / single-node mode

	mu          sync.Mutex
	currentTerm int64
	votedFor    string   // nodeID that received our vote this term; "" = none
	role        RaftRole
	leaderID    string   // nodeID of the current known leader; "" = unknown

	// Raft §5.3 Log state.
	// log is 0-indexed; log[i].Index == i+1 (1-based Raft indices).
	log         []LogEntry
	commitIndex int64 // highest log index known to be committed
	lastApplied int64 // highest log index applied to state machine

	// Leader-only volatile state (§5.3). Keyed by peer address.
	// Initialised to lastLogIndex+1 on election; reset on each term.
	nextIndex  map[string]int64 // next log index to send to each peer
	matchIndex map[string]int64 // highest index known to be replicated on each peer

	// meta persists currentTerm and votedFor across restarts (Raft §5 persistent
	// state). nil disables persistence — used in tests and single-node mode.
	meta MetaStore

	// logStore persists LogEntry records across restarts (Raft §5 persistent
	// state). nil disables persistence — used in tests and single-node mode.
	// Phase 5c: wire to WALLogStore in production.
	logStore LogStore

	// applyCh delivers committed log entries to the state machine consumer.
	// runApplyLoop sends here when commitIndex advances past lastApplied.
	// Buffer of 256: allows burst delivery without blocking the apply loop.
	applyCh chan LogEntry

	// resetCh is sent to from Handle* methods to reset the election timer.
	// Buffer of 1: sender never blocks.
	resetCh chan struct{}

	// sm is the KV state machine. Non-nil enables snapshot support.
	// When non-nil, maybeSnapshot() calls sm.TakeSnapshot().
	sm *KVStateMachine

	// Snapshot state — protected by mu.
	//
	// snapshotIndex is the last Raft log index covered by the most recent
	// durable snapshot (0 = no snapshot taken yet).
	// snapshotTerm is the term of that index.
	// These mirror the "last included index/term" from the Raft snapshot paper.
	snapshotIndex int64
	snapshotTerm  int64

	snapshotStore SnapshotStore  // nil disables snapshotting
	snapshotCfg   SnapshotConfig

	// snapshotInProgress is 0 when idle, 1 when a snapshot goroutine is running.
	// CAS is used to prevent concurrent snapshots.
	// zero-alloc: atomic.Int32 avoids a heap allocation vs sync.Mutex for this flag.
	snapshotInProgress atomic.Int32

	// snapBuf accumulates incoming snapshot chunks during an InstallSnapshot RPC.
	// Protected by snapBufMu (separate from mu to avoid holding mu across chunk I/O).
	snapBufMu sync.Mutex
	snapBuf   []byte
}

// NewRaftNode creates a RaftNode. peers may be nil for single-node or test use.
// meta may be nil to disable persistence (tests, single-node mode). When non-nil,
// the node restores currentTerm and votedFor from the store before Run() is called.
// logStore may be nil to disable log persistence. When non-nil, the node
// restores log entries from the store before Run() is called.
func NewRaftNode(id string, peers *PeerClients, meta MetaStore, logStore LogStore) *RaftNode {
	n := &RaftNode{
		id:       id,
		peers:    peers,
		role:     RoleFollower,
		meta:     meta,
		logStore: logStore,
		applyCh:  make(chan LogEntry, 256),
		resetCh:  make(chan struct{}, 1),
	}
	if meta != nil {
		if m, err := meta.Load(); err != nil {
			slog.Error("raft: failed to load persistent metadata, starting from zero state", "err", err)
		} else {
			n.currentTerm = m.Term
			n.votedFor = m.VotedFor
		}
	}
	if logStore != nil {
		entries, err := logStore.LoadAll()
		if err != nil {
			slog.Error("raft: failed to load persistent log, starting from empty log", "err", err)
		} else {
			n.log = entries
			slog.Info("raft: log restored from store", "entries", len(entries))
		}
	}
	return n
}

// Role returns the current Raft role. Safe to call from any goroutine.
func (n *RaftNode) Role() RaftRole {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.role
}

// Term returns the current term. Safe to call from any goroutine.
func (n *RaftNode) Term() int64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.currentTerm
}

// LeaderID returns the node ID of the current known leader, or "" if unknown.
// Safe to call from any goroutine.
func (n *RaftNode) LeaderID() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.leaderID
}

// RoleString returns the current role as a string. Used by metrics.
func (n *RaftNode) RoleString() string {
	return n.Role().String()
}

// ForceRole sets the role and term directly. Used only in tests.
func (n *RaftNode) ForceRole(role RaftRole, term int64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.role = role
	n.currentTerm = term
}

// ForceLeaderID sets the known leader ID directly. Used only in tests.
func (n *RaftNode) ForceLeaderID(id string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.leaderID = id
}

// ForceLog appends a synthetic log entry with the given index and term.
// Used only in tests to simulate a node that has already received log entries.
func (n *RaftNode) ForceLog(lastLogIndex, lastLogTerm int64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	// Replace the log with a single sentinel entry that has the requested
	// index/term. This is sufficient for election-restriction tests (§5.4.1)
	// without constructing a full sequence.
	n.log = []LogEntry{{Index: lastLogIndex, Term: lastLogTerm}}
}

// lastEntry returns the index and term of the last log entry.
// Must be called with mu held.
//
// Post-snapshot correctness (§5.4.1): when n.log is empty because all entries
// have been compacted into a snapshot, we return snapshotIndex/snapshotTerm so
// that election log-up-to-date checks and nextIndex calculations remain correct.
func (n *RaftNode) lastEntry() (index, term int64) {
	if len(n.log) == 0 {
		// No in-memory entries: use snapshot baseline (0,0 if no snapshot yet).
		return n.snapshotIndex, n.snapshotTerm
	}
	e := n.log[len(n.log)-1]
	return e.Index, e.Term
}

// entryAt returns the LogEntry at 1-based Raft index i, and whether it exists.
// Must be called with mu held.
//
// Post-snapshot safety: after CompactPrefix, n.log may start at an index well
// above 1 (e.g. [101, 102, 103] after a snapshot at 100). The old fast-reject
// `index > len(n.log)` would give a false negative in that case, so we guard
// only on index <= 0 and len(n.log) == 0, then fall through to the linear scan.
func (n *RaftNode) entryAt(index int64) (LogEntry, bool) {
	if index <= 0 || len(n.log) == 0 {
		return LogEntry{}, false
	}
	// Linear search from the end: O(1) for the common case (recent index).
	// log[i].Index may not equal i+1 after prefix compaction or ForceLog.
	for i := len(n.log) - 1; i >= 0; i-- {
		if n.log[i].Index == index {
			return n.log[i], true
		}
	}
	return LogEntry{}, false
}

// termAt returns the term for a given 1-based Raft index, or (0, false) if
// unknown. It checks the in-memory log first, then falls back to the snapshot
// baseline for the exact snapshotIndex boundary.
//
// Post-snapshot correctness: after CompactPrefix, entries with Index <=
// snapshotIndex are no longer in n.log. If the caller asks for snapshotIndex
// itself (the "prev" boundary of the first new batch), we must return
// snapshotTerm rather than 0 to avoid false log-mismatch rejections.
//
// Must be called with mu held.
func (n *RaftNode) termAt(index int64) (term int64, ok bool) {
	if index <= 0 {
		return 0, false
	}
	if e, found := n.entryAt(index); found {
		return e.Term, true
	}
	// Snapshot boundary: the exact last-included index is known even after
	// log compaction.
	if index == n.snapshotIndex {
		return n.snapshotTerm, true
	}
	return 0, false
}

// CommitIndex returns the current commitIndex. Safe to call from any goroutine.
func (n *RaftNode) CommitIndex() int64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.commitIndex
}

// LogLen returns the number of entries in the in-memory log. Used by tests.
func (n *RaftNode) LogLen() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.log)
}

// HandleAppendEntries processes an incoming AppendEntries RPC (§5.3).
// Implements RaftHandler.
//
// §5.3 Log Matching guarantees:
//   - If two entries in different logs have the same index and term,
//     they store the same command (ensured by leader never overwriting).
//   - If two entries in different logs have the same index and term,
//     all preceding entries are identical (enforced by prevLogIndex check here).
func (n *RaftNode) HandleAppendEntries(args AppendEntriesArgs) AppendEntriesResult {
	n.mu.Lock()
	defer n.mu.Unlock()

	reject := func(conflictIndex, conflictTerm int64) AppendEntriesResult {
		return AppendEntriesResult{
			Term:          n.currentTerm,
			Success:       false,
			ConflictIndex: conflictIndex,
			ConflictTerm:  conflictTerm,
		}
	}

	if args.Term < n.currentTerm {
		// Stale leader — reject. Fast Backup fields not meaningful here.
		return reject(0, 0)
	}

	// Valid RPC: update term and revert to Follower if necessary.
	if args.Term > n.currentTerm {
		n.currentTerm = args.Term
		n.votedFor = ""
		if n.meta != nil {
			if err := n.meta.Save(RaftMeta{Term: args.Term, VotedFor: ""}); err != nil {
				slog.Warn("raft: failed to persist term update from AppendEntries", "err", err)
			}
		}
	}
	n.role = RoleFollower
	n.leaderID = args.LeaderID

	// Signal the run loop to reset the election timer (non-blocking).
	select {
	case n.resetCh <- struct{}{}:
	default:
	}

	// §5.3 Consistency check: prevLogIndex/prevLogTerm must match.
	if args.PrevLogIndex > 0 {
		prevTerm, ok := n.termAt(args.PrevLogIndex)
		if !ok {
			// We don't have an entry at prevLogIndex (and it is not the
			// snapshot boundary). Fast Backup: tell the leader the length
			// of our log so it can jump directly to the next valid index.
			lastIdx, _ := n.lastEntry()
			return reject(lastIdx+1, 0)
		}
		if prevTerm != args.PrevLogTerm {
			// Term mismatch: find the first index of prevTerm so the leader
			// can skip the entire conflicting term in one round-trip.
			conflictTerm := prevTerm
			conflictIdx := args.PrevLogIndex
			for conflictIdx > 1 {
				t, ok := n.termAt(conflictIdx - 1)
				if !ok || t != conflictTerm {
					break
				}
				conflictIdx--
			}
			return reject(conflictIdx, conflictTerm)
		}
	}

	// §5.3: Append new entries, truncating conflicting ones first.
	// Once a conflict is found we truncate and switch to append-only mode;
	// re-checking entryAt after truncation is correct but O(n²).  Since we
	// process entries in ascending index order the truncation point never moves
	// backward, so a single pass is sufficient.
	truncated := false
	for _, pbEntry := range args.Entries {
		if !truncated {
			existing, ok := n.entryAt(pbEntry.Index)
			if ok && existing.Term == pbEntry.Term {
				// Matching entry already present: skip (idempotent).
				continue
			}
			if ok && existing.Term != pbEntry.Term {
				// Conflict: persist truncation marker before modifying in-memory log.
				// If persist fails, return failure without touching in-memory state;
				// the leader will retry and eventually converge.
				if n.logStore != nil {
					if err := n.logStore.TruncateSuffix(pbEntry.Index); err != nil {
						slog.Error("raft: failed to persist log truncation, rejecting AppendEntries", "err", err)
						return AppendEntriesResult{Term: n.currentTerm, Success: false}
					}
				}
				cutAt := 0
				for cutAt < len(n.log) && n.log[cutAt].Index < pbEntry.Index {
					cutAt++
				}
				n.log = n.log[:cutAt]
				truncated = true
				// Fall through to append below.
			}
			// ok=false: entry not present → append below.
		}
		// Persist the new entry before updating in-memory log.
		// If persist fails, the entry is in neither the WAL nor memory — consistent.
		// Return failure so the leader retries; it will re-send this entry.
		if n.logStore != nil {
			if err := n.logStore.Append(LogEntry{Index: pbEntry.Index, Term: pbEntry.Term, Data: pbEntry.Data}); err != nil {
				slog.Error("raft: failed to persist log entry, rejecting AppendEntries", "err", err)
				return AppendEntriesResult{Term: n.currentTerm, Success: false}
			}
		}
		n.log = append(n.log, LogEntry{
			Index: pbEntry.Index,
			Term:  pbEntry.Term,
			Data:  pbEntry.Data,
		})
	}

	// §5.3: Advance commitIndex if leaderCommit is ahead.
	if args.LeaderCommit > n.commitIndex {
		lastIdx, _ := n.lastEntry()
		if args.LeaderCommit < lastIdx {
			n.commitIndex = args.LeaderCommit
		} else {
			n.commitIndex = lastIdx
		}
	}

	return AppendEntriesResult{Term: n.currentTerm, Success: true}
}

// HandleRequestVote processes an incoming RequestVote RPC.
// Implements RaftHandler.
//
// Vote is granted only if both conditions hold (Raft §5.2 + §5.4.1):
//  1. Term check: candidate's term ≥ our currentTerm.
//  2. Log completeness (§5.4.1): candidate's log is at least as up-to-date
//     as ours — prevents a stale replica from winning an election and
//     overwriting committed entries.
func (n *RaftNode) HandleRequestVote(term int64, candidateID string, lastLogIndex, lastLogTerm int64) (currentTerm int64, voteGranted bool) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if term < n.currentTerm {
		return n.currentTerm, false
	}

	if term > n.currentTerm {
		// Step down: higher term seen.
		n.currentTerm = term
		n.votedFor = ""
		n.role = RoleFollower
		if n.meta != nil {
			if err := n.meta.Save(RaftMeta{Term: term, VotedFor: ""}); err != nil {
				slog.Warn("raft: failed to persist term update from RequestVote", "err", err)
			}
		}
	}

	// §5.4.1: deny vote if candidate's log is behind ours.
	if !n.candidateLogUpToDate(lastLogIndex, lastLogTerm) {
		return n.currentTerm, false
	}

	// Grant vote if we haven't voted for anyone else this term.
	if n.votedFor == "" || n.votedFor == candidateID {
		// Persist before updating in-memory state: if Save fails, deny the vote
		// to prevent a crash from causing double-voting (§5 persistent state).
		if n.meta != nil {
			if err := n.meta.Save(RaftMeta{Term: n.currentTerm, VotedFor: candidateID}); err != nil {
				slog.Error("raft: failed to persist vote, denying to preserve safety", "err", err)
				return n.currentTerm, false
			}
		}
		n.votedFor = candidateID
		// Reset election timer: we just heard from a valid Candidate.
		select {
		case n.resetCh <- struct{}{}:
		default:
		}
		return n.currentTerm, true
	}

	return n.currentTerm, false
}

// candidateLogUpToDate reports whether the candidate's log is at least as
// up-to-date as this node's log (Raft §5.4.1).
//
// "More up-to-date" is defined by comparing the last entries:
//   - Higher lastLogTerm wins unconditionally.
//   - Equal lastLogTerm: longer log (higher lastLogIndex) wins.
//
// Must be called with mu held.
func (n *RaftNode) candidateLogUpToDate(candidateLastLogIndex, candidateLastLogTerm int64) bool {
	ourIdx, ourTerm := n.lastEntry()
	if candidateLastLogTerm != ourTerm {
		return candidateLastLogTerm > ourTerm
	}
	return candidateLastLogIndex >= ourIdx
}

// ApplyCh returns the channel on which committed log entries are delivered in
// index order. Consumers must read promptly; a slow consumer stalls the apply
// loop once the 256-entry buffer is full.
func (n *RaftNode) ApplyCh() <-chan LogEntry {
	return n.applyCh
}

// SetStateMachine wires a KVStateMachine to the node for snapshot support.
// Must be called before Run(). When sm is non-nil and snapshotStore is set,
// maybeSnapshot() will be called periodically.
func (n *RaftNode) SetStateMachine(sm *KVStateMachine) {
	n.sm = sm
}

// SetSnapshotStore enables snapshot persistence. Must be called before Run().
// cfg.Threshold == 0 disables automatic snapshotting.
func (n *RaftNode) SetSnapshotStore(store SnapshotStore, cfg SnapshotConfig) {
	n.snapshotStore = store
	n.snapshotCfg = cfg
}

// maybeSnapshot checks whether a snapshot should be taken and, if so, launches
// a snapshot goroutine. It is safe to call from any goroutine; it must NOT be
// called with mu held.
//
// Snapshot trigger condition: capturedIndex - snapshotIndex >= Threshold.
// Using sm.lastApplied (not commitIndex) ensures only fully-applied state
// is captured — no uncommitted entries leak into the snapshot.
func (n *RaftNode) maybeSnapshot() {
	if n.snapshotStore == nil || n.snapshotCfg.Threshold <= 0 || n.sm == nil {
		return
	}
	// CAS: prevent concurrent snapshots.
	if !n.snapshotInProgress.CompareAndSwap(0, 1) {
		return
	}

	// Capture state machine snapshot (COW — safe outside mu).
	data, capturedIndex, err := n.sm.TakeSnapshot()
	if err != nil {
		n.snapshotInProgress.Store(0)
		slog.Error("raft: TakeSnapshot failed", "err", err)
		return
	}

	if capturedIndex == 0 {
		// Nothing has been applied yet; no snapshot to take.
		n.snapshotInProgress.Store(0)
		return
	}

	n.mu.Lock()
	delta := capturedIndex - n.snapshotIndex
	if delta < n.snapshotCfg.Threshold {
		// Not enough new entries since the last snapshot.
		n.mu.Unlock()
		n.snapshotInProgress.Store(0)
		return
	}

	// Resolve the term for capturedIndex.
	var capturedTerm int64
	if e, ok := n.entryAt(capturedIndex); ok {
		capturedTerm = e.Term
	} else if capturedIndex == n.snapshotIndex {
		capturedTerm = n.snapshotTerm
	} else {
		// capturedIndex is not in the log — already compacted in a previous
		// snapshot cycle. Skip to avoid creating a snapshot with term=0.
		n.mu.Unlock()
		n.snapshotInProgress.Store(0)
		return
	}
	n.mu.Unlock()

	meta := SnapshotMeta{
		Index:     capturedIndex,
		Term:      capturedTerm,
		CreatedAt: time.Now(),
	}

	go func() {
		defer n.snapshotInProgress.Store(0)

		// INV-S3: Save snapshot durably before compacting the log.
		if err := n.snapshotStore.Save(meta, data); err != nil {
			slog.Error("raft: snapshot save failed",
				"index", capturedIndex, "term", capturedTerm, "err", err)
			return
		}

		// Prune old snapshots (best-effort; failure is non-fatal).
		retain := n.snapshotCfg.RetainCount
		if retain < 1 {
			retain = 2 // safe default
		}
		_ = n.snapshotStore.Prune(retain)

		// Truncate in-memory log prefix.
		n.mu.Lock()
		n.truncateLogPrefix(capturedIndex, capturedTerm)
		n.mu.Unlock()

		// Compact WAL on disk (best-effort; snapshot already durable — failure
		// is recoverable at next restart because WAL replay will skip already-
		// snapshotted entries after RestoreSnapshot sets snapshotIndex).
		if n.logStore != nil {
			if err := n.logStore.CompactPrefix(capturedIndex); err != nil {
				slog.Error("raft: WAL compact prefix failed",
					"upToIndex", capturedIndex, "err", err)
			}
		}

		slog.Info("raft: snapshot complete",
			"index", capturedIndex, "term", capturedTerm)
	}()
}

// truncateLogPrefix removes log entries with Index <= upToIndex from n.log
// and updates snapshotIndex/snapshotTerm.
// Must be called with mu held.
//
// Explicit copy semantics: we allocate a fresh slice to release the memory
// of the truncated prefix (GC pressure reduction on large logs).
func (n *RaftNode) truncateLogPrefix(upToIndex, term int64) {
	cutoff := 0
	for i, e := range n.log {
		if e.Index <= upToIndex {
			cutoff = i + 1
		} else {
			break
		}
	}
	if cutoff > 0 {
		// Explicit copy: avoids retaining the backing array of the old slice.
		newLog := make([]LogEntry, len(n.log)-cutoff)
		copy(newLog, n.log[cutoff:])
		n.log = newLog
	}
	n.snapshotIndex = upToIndex
	n.snapshotTerm = term
}

// HandleInstallSnapshot applies a snapshot chunk from the leader (§7).
// On the final chunk (done=true) the snapshot is applied to the state machine
// and persisted to snapshotStore.
//
// Invariants enforced:
//   - INV-S1: snapshot index N implies entries [1..N] are committed and applied.
//   - §7 optimisation: if we already have the entry at LastIncludedIndex with
//     matching term and have applied it, only discard the log prefix.
//
// Must NOT be called with mu held.
func (n *RaftNode) HandleInstallSnapshot(args InstallSnapshotArgs) InstallSnapshotResult {
	n.mu.Lock()

	// §7: stale term — reject.
	if args.Term < n.currentTerm {
		term := n.currentTerm
		n.mu.Unlock()
		return InstallSnapshotResult{Term: term}
	}

	// Higher term: step down.
	if args.Term > n.currentTerm {
		n.currentTerm = args.Term
		n.votedFor = ""
		n.role = RoleFollower
		if n.meta != nil {
			if err := n.meta.Save(RaftMeta{Term: args.Term, VotedFor: ""}); err != nil {
				slog.Warn("raft: install snapshot: failed to persist term step-down", "err", err)
			}
		}
	}
	n.role = RoleFollower
	n.leaderID = args.LeaderID

	// Reset election timer (non-blocking).
	select {
	case n.resetCh <- struct{}{}:
	default:
	}

	currentTerm := n.currentTerm
	n.mu.Unlock()

	// Accumulate chunk into the in-memory buffer.
	n.snapBufMu.Lock()
	if args.Offset == 0 {
		// First chunk: (re)initialise buffer.
		n.snapBuf = make([]byte, 0, int64(len(args.Data))*10)
	}
	n.snapBuf = append(n.snapBuf, args.Data...)
	n.snapBufMu.Unlock()

	if !args.Done {
		return InstallSnapshotResult{Term: currentTerm}
	}

	// Last chunk — apply the snapshot.
	n.snapBufMu.Lock()
	buf := n.snapBuf
	n.snapBuf = nil
	n.snapBufMu.Unlock()

	// Deserialise using the ADR-017 §3 format (same as FileSnapshotStore).
	snapMeta, data, err := unmarshalSnapshotData(buf)
	if err != nil {
		slog.Error("raft: install snapshot: unmarshal failed", "err", err)
		return InstallSnapshotResult{Term: currentTerm}
	}
	// Cross-check: the decoded index/term must match what the leader announced.
	if snapMeta.Index != args.LastIncludedIndex || snapMeta.Term != args.LastIncludedTerm {
		slog.Error("raft: install snapshot: index/term mismatch in payload",
			"announced_index", args.LastIncludedIndex, "decoded_index", snapMeta.Index)
		return InstallSnapshotResult{Term: currentTerm}
	}

	n.mu.Lock()

	// §7 optimisation: if we already have the matching entry and the state machine
	// is up-to-date, just compact the prefix — no full restore needed.
	if e, ok := n.entryAt(args.LastIncludedIndex); ok &&
		e.Term == args.LastIncludedTerm &&
		n.sm != nil && n.sm.LastApplied() >= args.LastIncludedIndex {
		n.truncateLogPrefix(args.LastIncludedIndex, args.LastIncludedTerm)
		n.mu.Unlock()
		return InstallSnapshotResult{Term: currentTerm}
	}

	// Full restore: replace state machine state.
	if n.sm != nil {
		// Unlock around the potentially blocking RestoreSnapshot call.
		n.mu.Unlock()
		if err := n.sm.RestoreSnapshot(data, args.LastIncludedIndex); err != nil {
			slog.Error("raft: install snapshot: restore failed", "err", err)
			return InstallSnapshotResult{Term: currentTerm}
		}
		n.mu.Lock()
	}

	// Discard log; update snapshot metadata and commitIndex.
	n.log = nil
	n.snapshotIndex = args.LastIncludedIndex
	n.snapshotTerm = args.LastIncludedTerm
	if n.commitIndex < args.LastIncludedIndex {
		n.commitIndex = args.LastIncludedIndex
	}
	if n.lastApplied < args.LastIncludedIndex {
		n.lastApplied = args.LastIncludedIndex
	}
	n.mu.Unlock()

	// Persist the received snapshot to disk so recovery after a crash works.
	if n.snapshotStore != nil {
		meta := SnapshotMeta{
			Index:     args.LastIncludedIndex,
			Term:      args.LastIncludedTerm,
			CreatedAt: time.Now(),
		}
		if err := n.snapshotStore.Save(meta, data); err != nil {
			slog.Error("raft: install snapshot: store save failed", "err", err)
		} else {
			retain := n.snapshotCfg.RetainCount
			if retain < 1 {
				retain = 2
			}
			_ = n.snapshotStore.Prune(retain)
		}
	}

	slog.Info("raft: snapshot installed",
		"index", args.LastIncludedIndex,
		"term", args.LastIncludedTerm)
	return InstallSnapshotResult{Term: currentTerm}
}

// sendSnapshot sends the current snapshot to a lagging peer (§7).
// Called by sendHeartbeats when nextIndex[peer] <= snapshotIndex.
// Updates nextIndex/matchIndex on success.
func (n *RaftNode) sendSnapshot(peerID string) {
	if n.snapshotStore == nil || n.peers == nil {
		return
	}

	n.mu.Lock()
	meta, err := n.snapshotStore.Latest()
	if err != nil {
		n.mu.Unlock()
		slog.Warn("raft: send snapshot: no latest snapshot", "peer", peerID, "err", err)
		return
	}
	currentTerm := n.currentTerm
	nodeID := n.id
	n.mu.Unlock()

	_, data, err := n.snapshotStore.Load(meta.Index)
	if err != nil {
		slog.Error("raft: send snapshot: load failed", "peer", peerID, "err", err)
		return
	}

	if err := n.peers.InstallSnapshot(peerID, currentTerm, nodeID, meta, data); err != nil {
		slog.Warn("raft: send snapshot failed", "peer", peerID, "err", err)
		return
	}

	n.mu.Lock()
	if n.nextIndex[peerID] < meta.Index+1 {
		n.nextIndex[peerID] = meta.Index + 1
	}
	if n.matchIndex[peerID] < meta.Index {
		n.matchIndex[peerID] = meta.Index
	}
	n.mu.Unlock()

	slog.Info("raft: snapshot sent", "peer", peerID, "index", meta.Index)
}

// runSnapshotTicker fires maybeSnapshot at cfg.CheckInterval.
// Runs as a background goroutine inside Run(). Exits when ctx is cancelled.
func (n *RaftNode) runSnapshotTicker(ctx context.Context) {
	ticker := time.NewTicker(n.snapshotCfg.CheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n.maybeSnapshot()
		}
	}
}

// RecoverFromSnapshot restores the state machine and node snapshot metadata
// from the latest durable snapshot on disk.
//
// Call order (startup sequence):
//  1. NewRaftNode(...)
//  2. node.SetStateMachine(sm)
//  3. node.SetSnapshotStore(store, cfg)
//  4. node.RecoverFromSnapshot()          ← this method
//  5. filter WAL entries: keep Index > node.SnapshotIndex()
//  6. sm.RecoverFromStore(filteredEntries)
//  7. node.Run(ctx)
//
// Returns the snapshot index (0 if no snapshot was found — not an error).
// After this call, n.SnapshotIndex() reflects the restored baseline.
func (n *RaftNode) RecoverFromSnapshot() int64 {
	return n.recoverFromSnapshot()
}

// SnapshotIndex returns the last included index of the most recent snapshot.
// Safe to call from any goroutine.
func (n *RaftNode) SnapshotIndex() int64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.snapshotIndex
}

// recoverFromSnapshot restores state machine and node snapshot metadata from
// the latest durable snapshot. Must be called before WAL log replay so that
// only entries with Index > snapshotIndex are replayed.
//
// Returns the snapshot index (0 if no snapshot found — not an error).
func (n *RaftNode) recoverFromSnapshot() int64 {
	if n.snapshotStore == nil || n.sm == nil {
		return 0
	}

	latestMeta, err := n.snapshotStore.Latest()
	if err != nil {
		if err == ErrSnapshotNotFound {
			// No snapshots — fresh start.
			return 0
		}
		slog.Error("raft: failed to query latest snapshot during recovery", "err", err)
		return 0
	}

	_, data, err := n.snapshotStore.Load(latestMeta.Index)
	if err != nil {
		slog.Error("raft: failed to load latest snapshot during recovery",
			"index", latestMeta.Index, "err", err)
		return 0
	}

	if err := n.sm.RestoreSnapshot(data, latestMeta.Index); err != nil {
		slog.Error("raft: failed to restore snapshot into state machine",
			"index", latestMeta.Index, "err", err)
		return 0
	}

	n.snapshotIndex = latestMeta.Index
	n.snapshotTerm = latestMeta.Term

	// Filter n.log: discard entries already covered by the snapshot.
	// n.log was populated by NewRaftNode from the WAL before this call.
	filtered := n.log[:0]
	for _, e := range n.log {
		if e.Index > latestMeta.Index {
			filtered = append(filtered, e)
		}
	}
	if len(filtered) < len(n.log) {
		newLog := make([]LogEntry, len(filtered))
		copy(newLog, filtered)
		n.log = newLog
		slog.Info("raft: log entries before snapshot discarded",
			"snapshot_index", latestMeta.Index, "remaining", len(n.log))
	}

	slog.Info("raft: restored from snapshot",
		"index", latestMeta.Index, "term", latestMeta.Term)

	return latestMeta.Index
}

// Propose appends data to the leader's log and returns the assigned index and
// term. Returns isLeader=false when this node is not the current leader;
// callers should redirect the request to the actual leader.
//
// The entry is persisted (fsync'd) to logStore before this method returns.
// Commit happens asynchronously: read ApplyCh() to know when the entry is
// committed and applied.
func (n *RaftNode) Propose(data []byte) (index int64, term int64, isLeader bool) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.role != RoleLeader {
		return 0, 0, false
	}

	lastIdx, _ := n.lastEntry()
	newIndex := lastIdx + 1
	entry := LogEntry{Index: newIndex, Term: n.currentTerm, Data: data}

	// Persist before in-memory append: same invariant as HandleAppendEntries.
	// If persist fails, treat as non-leader so the caller retries or redirects.
	if n.logStore != nil {
		if err := n.logStore.Append(entry); err != nil {
			slog.Error("raft: leader failed to persist proposed entry", "index", newIndex, "err", err)
			return 0, 0, false
		}
	}

	n.log = append(n.log, entry)
	slog.Info("raft: entry proposed", "id", n.id, "index", newIndex, "term", n.currentTerm)

	return newIndex, n.currentTerm, true
}

// Run starts the Raft state machine. Blocks until ctx is cancelled.
// Call this in a dedicated goroutine.
func (n *RaftNode) Run(ctx context.Context) {
	go n.runApplyLoop(ctx)

	// Start snapshot ticker if configured.
	if n.snapshotStore != nil && n.snapshotCfg.CheckInterval > 0 && n.sm != nil {
		go n.runSnapshotTicker(ctx)
	}

	for {
		n.mu.Lock()
		role := n.role
		n.mu.Unlock()

		switch role {
		case RoleFollower:
			n.runFollower(ctx)
		case RoleCandidate:
			n.runCandidate(ctx)
		case RoleLeader:
			n.runLeader(ctx)
		}

		if ctx.Err() != nil {
			return
		}
	}
}

// runApplyLoop delivers committed entries to applyCh.
//
// Polls at applyPollInterval; when commitIndex > lastApplied it collects
// entries [lastApplied+1, commitIndex] and sends them to applyCh in order.
// Runs until ctx is cancelled.
func (n *RaftNode) runApplyLoop(ctx context.Context) {
	ticker := time.NewTicker(applyPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n.mu.Lock()
			if n.commitIndex <= n.lastApplied {
				n.mu.Unlock()
				continue
			}
			// Snapshot the range to apply while holding the lock.
			from := n.lastApplied + 1
			to := n.commitIndex
			toApply := make([]LogEntry, 0, int(to-from+1))
			for i := from; i <= to; i++ {
				if e, ok := n.entryAt(i); ok {
					toApply = append(toApply, e)
				}
			}
			n.lastApplied = to
			n.mu.Unlock()

			for _, e := range toApply {
				select {
				case n.applyCh <- e:
				case <-ctx.Done():
					return
				}
			}
		}
	}
}

// runFollower runs the Follower state until election timeout or ctx cancel.
func (n *RaftNode) runFollower(ctx context.Context) {
	timer := time.NewTimer(randomElectionTimeout())
	defer timer.Stop()

	slog.Info("raft: follower", "id", n.id, "term", n.Term())

	for {
		select {
		case <-ctx.Done():
			return
		case <-n.resetCh:
			// Heartbeat or vote grant received: reset timer.
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(randomElectionTimeout())
		case <-timer.C:
			// Election timeout: become Candidate.
			n.mu.Lock()
			n.role = RoleCandidate
			n.mu.Unlock()
			return
		}
	}
}

// randomElectionTimeout returns a random duration in [ElectionTimeoutMin, ElectionTimeoutMax].
func randomElectionTimeout() time.Duration {
	delta := ElectionTimeoutMax - ElectionTimeoutMin
	//nolint:gosec // non-cryptographic random is correct for Raft election timeouts
	return ElectionTimeoutMin + time.Duration(rand.Int63n(int64(delta)))
}

// runCandidate runs the Candidate state:
//  1. Increment term and vote for self.
//  2. Send RequestVote to all peers concurrently.
//  3. If majority votes received → become Leader.
//  4. If AppendEntries from valid Leader received (via resetCh) → revert to Follower.
//  5. If election timer fires without majority → new election (stay Candidate).
func (n *RaftNode) runCandidate(ctx context.Context) {
	n.mu.Lock()
	n.currentTerm++
	term := n.currentTerm
	lastLogIndex, lastLogTerm := n.lastEntry()
	n.votedFor = n.id
	n.role = RoleCandidate
	if n.meta != nil {
		if err := n.meta.Save(RaftMeta{Term: term, VotedFor: n.id}); err != nil {
			// Cannot guarantee we won't re-vote in this term after a crash.
			// Abort the election to avoid a potential safety violation.
			slog.Error("raft: failed to persist candidate state, aborting election", "err", err)
			n.currentTerm--
			n.votedFor = ""
			n.role = RoleFollower
			n.mu.Unlock()
			return
		}
	}
	n.mu.Unlock()

	slog.Info("raft: starting election", "id", n.id, "term", term)

	var peers []*RaftClient
	if n.peers != nil {
		peers = n.peers.All()
	}
	total := len(peers) + 1 // +1 for self
	majority := total/2 + 1
	votes := 1 // self vote

	type voteResult struct {
		term    int64
		granted bool
	}
	resultCh := make(chan voteResult, len(peers))

	for _, peer := range peers {
		peer := peer
		go func() {
			resp, err := peer.RequestVote(term, n.id, lastLogIndex, lastLogTerm)
			if err != nil {
				resultCh <- voteResult{}
				return
			}
			resultCh <- voteResult{term: resp.Term, granted: resp.VoteGranted}
		}()
	}

	// Single-node fast path: no peers to query, self vote is already a majority.
	if votes >= majority {
		n.mu.Lock()
		if n.currentTerm == term {
			n.role = RoleLeader
		}
		n.mu.Unlock()
		slog.Info("raft: elected leader (single node)", "id", n.id, "term", term)
		return
	}

	timer := time.NewTimer(randomElectionTimeout())
	defer timer.Stop()

	collected := 0
	for collected < len(peers) {
		select {
		case <-ctx.Done():
			return
		case <-n.resetCh:
			// AppendEntries from valid Leader: step down.
			n.mu.Lock()
			n.role = RoleFollower
			n.mu.Unlock()
			slog.Info("raft: stepping down during election (heard from leader)", "id", n.id)
			return
		case <-timer.C:
			// Split vote or timeout: start new election.
			slog.Info("raft: election timed out, restarting", "id", n.id, "term", term)
			return // role stays Candidate, Run() calls runCandidate again
		case res := <-resultCh:
			collected++
			if res.term > term {
				n.mu.Lock()
				n.currentTerm = res.term
				n.votedFor = ""
				n.role = RoleFollower
				n.mu.Unlock()
				slog.Info("raft: stepping down (higher term in vote response)", "id", n.id, "term", res.term)
				return
			}
			if res.granted {
				votes++
			}
			if votes >= majority {
				n.mu.Lock()
				if n.currentTerm == term {
					n.role = RoleLeader
				}
				n.mu.Unlock()
				slog.Info("raft: elected leader", "id", n.id, "term", term, "votes", votes)
				return
			}
		}
	}
}

// runLeader runs the Leader state:
//  1. Initialise nextIndex[] and matchIndex[] per §5.3.
//  2. Send AppendEntries to all peers every HeartbeatInterval.
//  3. If any peer responds with higher term → step down to Follower.
//  4. Exits when ctx is cancelled or role changes externally.
func (n *RaftNode) runLeader(ctx context.Context) {
	n.mu.Lock()
	term := n.currentTerm
	lastIdx, _ := n.lastEntry()
	// Initialise leader volatile state (§5.3).
	// nextIndex: optimistic — assume peer is fully caught up.
	// matchIndex: pessimistic — assume nothing has been confirmed.
	if n.peers != nil {
		n.nextIndex = make(map[string]int64, len(n.peers.clients))
		n.matchIndex = make(map[string]int64, len(n.peers.clients))
		for addr := range n.peers.clients {
			n.nextIndex[addr] = lastIdx + 1
			n.matchIndex[addr] = 0
		}
	}
	n.leaderID = n.id
	n.mu.Unlock()

	slog.Info("raft: became leader", "id", n.id, "term", term)

	ticker := time.NewTicker(HeartbeatInterval)
	defer ticker.Stop()

	var peers []*RaftClient
	if n.peers != nil {
		peers = n.peers.All()
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n.mu.Lock()
			// Check if we've been demoted externally (e.g. HandleAppendEntries from higher-term leader).
			if n.role != RoleLeader || n.currentTerm != term {
				n.mu.Unlock()
				return
			}
			currentTerm := n.currentTerm
			n.mu.Unlock()

			n.sendHeartbeats(ctx, peers, currentTerm)

			// Single-node fast-path: no peers to replicate to, so sendHeartbeats
			// returns immediately without calling maybeAdvanceCommitIndex.
			// Advance commitIndex here so Propose'd entries get committed and
			// delivered to applyCh without waiting for a quorum that will never form.
			if len(peers) == 0 {
				n.mu.Lock()
				if n.role == RoleLeader && n.currentTerm == term {
					lastIdx, _ := n.lastEntry()
					n.maybeAdvanceCommitIndex(term, lastIdx, 1)
				}
				n.mu.Unlock()
			}
		}
	}
}

// sendHeartbeats sends AppendEntries to all peers concurrently.
// For Phase 5b, each call may carry log entries for peers that are behind
// (nextIndex[peer] ≤ lastLogIndex). If any peer returns a higher term, this
// node steps down. Successful replication advances matchIndex and may advance
// commitIndex when a quorum is reached (§5.4.2: only entries from currentTerm).
func (n *RaftNode) sendHeartbeats(ctx context.Context, peers []*RaftClient, term int64) {
	if len(peers) == 0 {
		return
	}

	type result struct {
		addr     string
		respTerm int64
		resp     *pb.AppendEntriesResponse
		// prevLogIndex of the batch we sent, so we can update nextIndex on failure.
		prevLogIndex int64
		// lastSentIndex is the highest index in the batch we sent (0 for heartbeat).
		lastSentIndex int64
		err           error
	}
	ch := make(chan result, len(peers))

	n.mu.Lock()
	lastIdx, _ := n.lastEntry()
	// Snapshot the entries we'll send per peer.
	type peerArgs struct {
		client       *RaftClient
		addr         string
		args         AppendEntriesArgs
		lastSentIdx  int64
	}
	sends := make([]peerArgs, 0, len(peers))
	// snapshotIdx is captured here to avoid repeated lock acquisitions inside
	// the goroutines spawned for snapshot sends below.
	snapshotIdx := n.snapshotIndex

	for _, peer := range peers {
		ni := n.nextIndex[peer.addr]

		// §7: if the peer is too far behind, send the snapshot instead of log entries.
		if n.snapshotStore != nil && ni <= snapshotIdx {
			// Launch snapshot goroutine; do not add to sends (no AppendEntries needed).
			peerAddr := peer.addr
			go n.sendSnapshot(peerAddr)
			continue
		}

		prevIdx := ni - 1
		var prevTerm int64
		if prevIdx > 0 {
			// termAt covers both in-log entries and the snapshotIndex boundary
			// so that after log compaction the prevTerm is not silently 0.
			if t, ok := n.termAt(prevIdx); ok {
				prevTerm = t
			}
		}
		var entries []*pb.LogEntry
		lastSentIdx := int64(0)
		for _, e := range n.log {
			if e.Index >= ni {
				entries = append(entries, &pb.LogEntry{
					Index: e.Index,
					Term:  e.Term,
					Data:  e.Data,
				})
				lastSentIdx = e.Index
			}
		}
		sends = append(sends, peerArgs{
			client: peer,
			addr:   peer.addr,
			args: AppendEntriesArgs{
				Term:         term,
				LeaderID:     n.id,
				PrevLogIndex: prevIdx,
				PrevLogTerm:  prevTerm,
				Entries:      entries,
				LeaderCommit: n.commitIndex,
			},
			lastSentIdx: lastSentIdx,
		})
	}
	n.mu.Unlock()

	for _, s := range sends {
		s := s
		go func() {
			resp, err := s.client.AppendEntries(s.args)
			if err != nil {
				ch <- result{addr: s.addr, err: err, prevLogIndex: s.args.PrevLogIndex, lastSentIndex: s.lastSentIdx}
				return
			}
			ch <- result{addr: s.addr, respTerm: resp.Term, resp: resp, prevLogIndex: s.args.PrevLogIndex, lastSentIndex: s.lastSentIdx}
		}()
	}

	total := len(peers) + 1 // +1 for self
	majority := total/2 + 1

	// Only collect responses from peers that got AppendEntries (sends).
	// Peers that were routed to sendSnapshot do not write to ch.
	for range sends {
		select {
		case <-ctx.Done():
			return
		case res := <-ch:
			if res.err != nil {
				continue
			}
			if res.respTerm > term {
				n.mu.Lock()
				if res.respTerm > n.currentTerm {
					n.currentTerm = res.respTerm
					n.votedFor = ""
				}
				n.role = RoleFollower
				n.mu.Unlock()
				slog.Info("raft: leader stepping down (higher term in heartbeat response)",
					"id", n.id, "new_term", res.respTerm)
				return
			}
			if res.resp == nil {
				continue
			}
			n.mu.Lock()
			if res.resp.Success {
				// Update matchIndex and nextIndex for the peer.
				if res.lastSentIndex > n.matchIndex[res.addr] {
					n.matchIndex[res.addr] = res.lastSentIndex
				}
				n.nextIndex[res.addr] = n.matchIndex[res.addr] + 1

				// §5.4.2: advance commitIndex if a quorum has replicated an
				// entry from the currentTerm.
				n.maybeAdvanceCommitIndex(term, lastIdx, majority)
			} else {
				// Fast Backup: jump nextIndex using conflictIndex/conflictTerm.
				ci := res.resp.ConflictIndex
				ct := res.resp.ConflictTerm
				if ct == 0 || ci == 0 {
					// Follower has no entry at prevLogIndex: jump to ci.
					if ci > 0 {
						n.nextIndex[res.addr] = ci
					} else {
						n.nextIndex[res.addr] = 1
					}
				} else {
					// Find the last index in our log with term == ct.
					// If we have it, set nextIndex to the index after our last ct entry.
					// If we don't have it, set nextIndex to ci.
					found := false
					for i := len(n.log) - 1; i >= 0; i-- {
						if n.log[i].Term == ct {
							n.nextIndex[res.addr] = n.log[i].Index + 1
							found = true
							break
						}
					}
					if !found {
						n.nextIndex[res.addr] = ci
					}
				}
				// nextIndex must always be at least 1.
				if n.nextIndex[res.addr] < 1 {
					n.nextIndex[res.addr] = 1
				}
			}
			n.mu.Unlock()
		}
	}
}

// maybeAdvanceCommitIndex advances commitIndex to the highest N such that:
//   - N > commitIndex
//   - a majority of nodes have matchIndex[peer] ≥ N
//   - log[N].term == currentTerm  (§5.4.2 safety: never commit from prior terms alone)
//
// Must be called with mu held.
func (n *RaftNode) maybeAdvanceCommitIndex(currentTerm, lastIdx int64, majority int) {
	for N := lastIdx; N > n.commitIndex; N-- {
		e, ok := n.entryAt(N)
		if !ok || e.Term != currentTerm {
			continue
		}
		// Count replicas that have matched at least N (self always counts).
		count := 1
		for _, m := range n.matchIndex {
			if m >= N {
				count++
			}
		}
		if count >= majority {
			n.commitIndex = N
			slog.Info("raft: commitIndex advanced", "id", n.id, "commitIndex", N)
			break
		}
	}
}
