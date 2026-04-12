package raft_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/junyoung/core-x/internal/infrastructure/raft"
	pb "github.com/junyoung/core-x/proto/pb"
)

func TestRaftRoleString(t *testing.T) {
	tests := []struct {
		role raft.RaftRole
		want string
	}{
		{raft.RoleFollower, "follower"},
		{raft.RoleCandidate, "candidate"},
		{raft.RoleLeader, "leader"},
	}
	for _, tc := range tests {
		if got := tc.role.String(); got != tc.want {
			t.Errorf("role %d: got %q, want %q", tc.role, got, tc.want)
		}
	}
}

func TestRaftNode_StartsAsFollower(t *testing.T) {
	node := raft.NewRaftNode("node-1", nil, nil)
	if got := node.Role(); got != raft.RoleFollower {
		t.Errorf("expected RoleFollower, got %v", got)
	}
	if got := node.Term(); got != 0 {
		t.Errorf("expected term 0, got %d", got)
	}
}

func TestRaftNode_HandleAppendEntries_HigherTerm_UpdatesTerm(t *testing.T) {
	node := raft.NewRaftNode("node-1", nil, nil)

	res := node.HandleAppendEntries(raft.AppendEntriesArgs{Term: 5, LeaderID: "node-2"})
	if !res.Success {
		t.Error("expected success=true")
	}
	if res.Term != 5 {
		t.Errorf("expected returned term=5, got %d", res.Term)
	}
	if got := node.Term(); got != 5 {
		t.Errorf("expected node term=5, got %d", got)
	}
	if got := node.Role(); got != raft.RoleFollower {
		t.Errorf("expected RoleFollower after AppendEntries, got %v", got)
	}
}

func TestRaftNode_HandleAppendEntries_LowerTerm_Rejected(t *testing.T) {
	node := raft.NewRaftNode("node-1", nil, nil)
	node.HandleAppendEntries(raft.AppendEntriesArgs{Term: 10, LeaderID: "node-2"}) // advance term to 10

	res := node.HandleAppendEntries(raft.AppendEntriesArgs{Term: 5, LeaderID: "node-3"}) // stale leader
	if res.Success {
		t.Error("expected success=false for stale term")
	}
	if res.Term != 10 {
		t.Errorf("expected current term 10 in response, got %d", res.Term)
	}
}

func TestRaftNode_HandleRequestVote_GrantsOnce(t *testing.T) {
	node := raft.NewRaftNode("node-1", nil, nil)

	// First request: should grant.
	_, granted1 := node.HandleRequestVote(1, "node-2", 0, 0)
	if !granted1 {
		t.Error("expected first vote to be granted")
	}

	// Second request from different candidate same term: should deny.
	_, granted2 := node.HandleRequestVote(1, "node-3", 0, 0)
	if granted2 {
		t.Error("expected second vote from different candidate to be denied")
	}
}

func TestRaftNode_HandleRequestVote_HigherTermResetsVote(t *testing.T) {
	node := raft.NewRaftNode("node-1", nil, nil)

	// Vote for node-2 in term 1.
	node.HandleRequestVote(1, "node-2", 0, 0)

	// Higher term: vote should reset, node-3 should get the vote.
	_, granted := node.HandleRequestVote(2, "node-3", 0, 0)
	if !granted {
		t.Error("expected vote grant in new higher term")
	}
	if got := node.Term(); got != 2 {
		t.Errorf("expected term=2, got %d", got)
	}
}

func TestRaftNode_SingleNode_BecomesLeader(t *testing.T) {
	node := raft.NewRaftNode("node-1", nil, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Run the state machine in background
	done := make(chan struct{})
	go func() {
		node.Run(ctx)
		close(done)
	}()

	// Wait for Leader election (should happen within one election timeout ~300ms max)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if node.Role() == raft.RoleLeader {
			cancel() // stop the run loop
			<-done
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	t.Errorf("expected single node to become leader within 1s, final role: %v", node.Role())
}

// §5.4.1 Election Restriction tests.
// These guard against a stale replica winning an election and overwriting
// committed entries that it never received.

func TestRaftNode_HandleRequestVote_StaleLogTerm_Denied(t *testing.T) {
	node := raft.NewRaftNode("node-1", nil, nil)
	// Simulate node-1 having log up to (index=3, term=4).
	node.ForceLog(3, 4)

	// Candidate only has log up to (index=5, term=3) — longer but older term.
	_, granted := node.HandleRequestVote(5, "node-2", 5, 3)
	if granted {
		t.Error("expected vote denied: candidate's lastLogTerm(3) < our lastLogTerm(4)")
	}
}

func TestRaftNode_HandleRequestVote_SameTermShorterLog_Denied(t *testing.T) {
	node := raft.NewRaftNode("node-1", nil, nil)
	// node-1 has (index=5, term=4).
	node.ForceLog(5, 4)

	// Candidate has (index=3, term=4) — same term but shorter log.
	_, granted := node.HandleRequestVote(5, "node-2", 3, 4)
	if granted {
		t.Error("expected vote denied: same lastLogTerm but candidate lastLogIndex(3) < ours(5)")
	}
}

func TestRaftNode_HandleRequestVote_SameTermEqualLog_Granted(t *testing.T) {
	node := raft.NewRaftNode("node-1", nil, nil)
	node.ForceLog(5, 4)

	_, granted := node.HandleRequestVote(5, "node-2", 5, 4)
	if !granted {
		t.Error("expected vote granted: candidate log identical to ours")
	}
}

func TestRaftNode_HandleRequestVote_NewerLogTerm_Granted(t *testing.T) {
	node := raft.NewRaftNode("node-1", nil, nil)
	node.ForceLog(10, 3)

	// Candidate has shorter log but higher term — wins by §5.4.1 term rule.
	_, granted := node.HandleRequestVote(5, "node-2", 2, 4)
	if !granted {
		t.Error("expected vote granted: candidate lastLogTerm(4) > our lastLogTerm(3)")
	}
}

func TestRaftNode_HandleAppendEntries_ResetsToFollower_WhenLeader(t *testing.T) {
	node := raft.NewRaftNode("node-1", nil, nil)

	node.ForceRole(raft.RoleLeader, 3)

	if got := node.Role(); got != raft.RoleLeader {
		t.Fatalf("expected RoleLeader, got %v", got)
	}

	// Higher-term AppendEntries should demote to Follower.
	res := node.HandleAppendEntries(raft.AppendEntriesArgs{Term: 4, LeaderID: "node-3"})
	if !res.Success {
		t.Error("expected success=true")
	}
	if got := node.Role(); got != raft.RoleFollower {
		t.Errorf("expected RoleFollower after higher-term heartbeat, got %v", got)
	}
}

// §5 Persistent State tests.
// These guard against term/vote loss across restarts, which could allow
// a node to cast two votes in the same term or accept a stale leader.

func TestFileMetaStore_SaveLoad_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "raft_meta.bin")
	store, err := raft.NewFileMetaStore(path)
	if err != nil {
		t.Fatalf("NewFileMetaStore: %v", err)
	}
	defer store.Close()

	want := raft.RaftMeta{Term: 42, VotedFor: "node-7"}
	if err := store.Save(want); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != want {
		t.Errorf("Load() = %+v, want %+v", got, want)
	}
}

func TestFileMetaStore_Load_EmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "raft_meta.bin")
	store, err := raft.NewFileMetaStore(path)
	if err != nil {
		t.Fatalf("NewFileMetaStore: %v", err)
	}
	defer store.Close()

	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load on empty file should not error: %v", err)
	}
	if got.Term != 0 || got.VotedFor != "" {
		t.Errorf("expected zero RaftMeta, got %+v", got)
	}
}

func TestFileMetaStore_Load_CRCMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "raft_meta.bin")
	store, err := raft.NewFileMetaStore(path)
	if err != nil {
		t.Fatalf("NewFileMetaStore: %v", err)
	}
	if err := store.Save(raft.RaftMeta{Term: 1, VotedFor: "node-1"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	store.Close()

	// Corrupt a byte in the middle of the record.
	f, err := os.OpenFile(path, os.O_RDWR, 0600)
	if err != nil {
		t.Fatalf("open for corruption: %v", err)
	}
	if _, err := f.WriteAt([]byte{0xFF}, 5); err != nil {
		t.Fatalf("corrupt write: %v", err)
	}
	f.Close()

	store2, err := raft.NewFileMetaStore(path)
	if err != nil {
		t.Fatalf("NewFileMetaStore (reopen): %v", err)
	}
	defer store2.Close()

	if _, err := store2.Load(); err == nil {
		t.Error("expected CRC error on corrupted file, got nil")
	}
}

func TestRaftNode_Meta_PersistsVoteOnGrant(t *testing.T) {
	meta := raft.NewMemMetaStore()
	node := raft.NewRaftNode("node-1", nil, meta)

	_, granted := node.HandleRequestVote(1, "node-2", 0, 0)
	if !granted {
		t.Fatal("expected vote granted")
	}

	saved, _ := meta.Load()
	if saved.Term != 1 {
		t.Errorf("expected saved term=1, got %d", saved.Term)
	}
	if saved.VotedFor != "node-2" {
		t.Errorf("expected saved votedFor=node-2, got %q", saved.VotedFor)
	}
}

func TestRaftNode_Meta_PersistsTermOnAppendEntries(t *testing.T) {
	meta := raft.NewMemMetaStore()
	node := raft.NewRaftNode("node-1", nil, meta)

	node.HandleAppendEntries(raft.AppendEntriesArgs{Term: 7, LeaderID: "leader-1"})

	saved, _ := meta.Load()
	if saved.Term != 7 {
		t.Errorf("expected saved term=7, got %d", saved.Term)
	}
	if saved.VotedFor != "" {
		t.Errorf("expected saved votedFor cleared, got %q", saved.VotedFor)
	}
}

func TestRaftNode_Meta_DeniesVoteOnSaveFailure(t *testing.T) {
	meta := raft.NewMemMetaStore()
	meta.InjectSaveError(errors.New("disk full"))

	node := raft.NewRaftNode("node-1", nil, meta)

	_, granted := node.HandleRequestVote(1, "node-2", 0, 0)
	if granted {
		t.Error("expected vote denied when Save fails (safety over availability)")
	}
}

func TestRaftNode_Meta_RestoredOnRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "raft_meta.bin")

	// First lifetime: node votes in term 3.
	store1, err := raft.NewFileMetaStore(path)
	if err != nil {
		t.Fatalf("NewFileMetaStore: %v", err)
	}
	node1 := raft.NewRaftNode("node-1", nil, store1)
	node1.HandleRequestVote(3, "node-2", 0, 0)
	store1.Close()

	// Second lifetime: new node with same file — must restore term and vote.
	store2, err := raft.NewFileMetaStore(path)
	if err != nil {
		t.Fatalf("NewFileMetaStore (reopen): %v", err)
	}
	defer store2.Close()
	node2 := raft.NewRaftNode("node-1", nil, store2)

	if got := node2.Term(); got != 3 {
		t.Errorf("expected restored term=3, got %d", got)
	}
	// A vote already cast in term 3 must not be re-issued to a different candidate.
	_, granted := node2.HandleRequestVote(3, "node-3", 0, 0)
	if granted {
		t.Error("expected vote denied: already voted for node-2 in term 3")
	}
}

// ---------------------------------------------------------------------------
// §5.3 Log Matching Property tests
// ---------------------------------------------------------------------------

// makeEntry is a test helper that constructs a pb.LogEntry.
func makeEntry(index, term int64, data string) *pb.LogEntry {
	return &pb.LogEntry{Index: index, Term: term, Data: []byte(data)}
}

// aeArgs is a test helper to build AppendEntriesArgs with sensible defaults.
func aeArgs(term int64, leaderID string, prevIdx, prevTerm int64, entries []*pb.LogEntry, leaderCommit int64) raft.AppendEntriesArgs {
	return raft.AppendEntriesArgs{
		Term:         term,
		LeaderID:     leaderID,
		PrevLogIndex: prevIdx,
		PrevLogTerm:  prevTerm,
		Entries:      entries,
		LeaderCommit: leaderCommit,
	}
}

// TestAppendEntries_Heartbeat_NoEntries verifies that an empty AppendEntries
// (heartbeat) is accepted and resets the follower without touching the log.
func TestAppendEntries_Heartbeat_NoEntries(t *testing.T) {
	node := raft.NewRaftNode("node-1", nil, nil)
	node.ForceRole(raft.RoleCandidate, 2)

	res := node.HandleAppendEntries(aeArgs(2, "leader", 0, 0, nil, 0))
	if !res.Success {
		t.Fatal("heartbeat should succeed")
	}
	if node.Role() != raft.RoleFollower {
		t.Error("expected follower after heartbeat")
	}
	if node.LogLen() != 0 {
		t.Errorf("expected empty log, got %d entries", node.LogLen())
	}
}

// TestAppendEntries_AppendNewEntries verifies that new entries are appended
// when prevLogIndex is consistent (base case: appending to empty log).
func TestAppendEntries_AppendNewEntries(t *testing.T) {
	node := raft.NewRaftNode("node-1", nil, nil)

	entries := []*pb.LogEntry{
		makeEntry(1, 1, "cmd-a"),
		makeEntry(2, 1, "cmd-b"),
	}
	res := node.HandleAppendEntries(aeArgs(1, "leader", 0, 0, entries, 0))
	if !res.Success {
		t.Fatalf("expected success, got failure (conflictIndex=%d conflictTerm=%d)", res.ConflictIndex, res.ConflictTerm)
	}
	if got := node.LogLen(); got != 2 {
		t.Errorf("expected 2 log entries, got %d", got)
	}
}

// TestAppendEntries_PrevLogIndex_Miss_ReturnsConflictIndex verifies that when
// the follower lacks the entry at prevLogIndex, it returns the length of its
// log as conflictIndex so the leader can jump forward (Fast Backup §5.3).
func TestAppendEntries_PrevLogIndex_Miss_ReturnsConflictIndex(t *testing.T) {
	node := raft.NewRaftNode("node-1", nil, nil)
	// Node has only entry at index 1, term 1.
	node.HandleAppendEntries(aeArgs(1, "leader", 0, 0, []*pb.LogEntry{makeEntry(1, 1, "a")}, 0))

	// Leader thinks peer has up to index 3; prevLogIndex=3 does not exist.
	res := node.HandleAppendEntries(aeArgs(1, "leader", 3, 1, nil, 0))
	if res.Success {
		t.Fatal("expected failure: prevLogIndex=3 not in log")
	}
	// conflictIndex should be 2 (lastLogIndex+1 = 1+1).
	if res.ConflictIndex != 2 {
		t.Errorf("expected conflictIndex=2, got %d", res.ConflictIndex)
	}
	if res.ConflictTerm != 0 {
		t.Errorf("expected conflictTerm=0 (no entry), got %d", res.ConflictTerm)
	}
}

// TestAppendEntries_PrevLogTerm_Mismatch_ReturnsConflictTerm verifies that
// when the entry at prevLogIndex exists but has the wrong term, the follower
// returns the conflicting term and its first index (Fast Backup).
func TestAppendEntries_PrevLogTerm_Mismatch_ReturnsConflictTerm(t *testing.T) {
	node := raft.NewRaftNode("node-1", nil, nil)
	// Build log: [1:t1, 2:t1, 3:t1] — all in term 1.
	node.HandleAppendEntries(aeArgs(1, "leader", 0, 0, []*pb.LogEntry{
		makeEntry(1, 1, "a"),
		makeEntry(2, 1, "b"),
		makeEntry(3, 1, "c"),
	}, 0))

	// Leader sends with prevLogIndex=3, prevLogTerm=2 (mismatch: we have term 1).
	res := node.HandleAppendEntries(aeArgs(2, "leader", 3, 2, nil, 0))
	if res.Success {
		t.Fatal("expected failure: prevLogTerm mismatch")
	}
	if res.ConflictTerm != 1 {
		t.Errorf("expected conflictTerm=1, got %d", res.ConflictTerm)
	}
	// conflictIndex should be 1 (first index with term 1).
	if res.ConflictIndex != 1 {
		t.Errorf("expected conflictIndex=1, got %d", res.ConflictIndex)
	}
}

// TestAppendEntries_ConflictTruncation verifies that conflicting entries are
// deleted and new entries replace them (§5.3: "delete the existing entry and
// all that follow it").
func TestAppendEntries_ConflictTruncation(t *testing.T) {
	node := raft.NewRaftNode("node-1", nil, nil)
	// Start with log [1:t1, 2:t1, 3:t1].
	node.HandleAppendEntries(aeArgs(1, "leader", 0, 0, []*pb.LogEntry{
		makeEntry(1, 1, "a"),
		makeEntry(2, 1, "b"),
		makeEntry(3, 1, "c"),
	}, 0))

	// Leader (term 2) sends [3:t2, 4:t2] starting at prevLogIndex=2.
	// Entry 3 in our log has term 1 — conflict → truncate [3:t1], replace with [3:t2, 4:t2].
	res := node.HandleAppendEntries(aeArgs(2, "leader", 2, 1, []*pb.LogEntry{
		makeEntry(3, 2, "c-new"),
		makeEntry(4, 2, "d"),
	}, 0))
	if !res.Success {
		t.Fatalf("expected success after truncation, got failure (conflictIndex=%d conflictTerm=%d)", res.ConflictIndex, res.ConflictTerm)
	}
	if got := node.LogLen(); got != 4 {
		t.Errorf("expected 4 log entries after replace, got %d", got)
	}
}

// TestAppendEntries_IdempotentReplay verifies that replaying the same entries
// does not duplicate them (§5.3 Log Matching: if index+term match, same command).
func TestAppendEntries_IdempotentReplay(t *testing.T) {
	node := raft.NewRaftNode("node-1", nil, nil)
	entries := []*pb.LogEntry{makeEntry(1, 1, "a"), makeEntry(2, 1, "b")}
	node.HandleAppendEntries(aeArgs(1, "leader", 0, 0, entries, 0))
	// Replay the same entries.
	res := node.HandleAppendEntries(aeArgs(1, "leader", 0, 0, entries, 0))
	if !res.Success {
		t.Fatal("replay should succeed")
	}
	if got := node.LogLen(); got != 2 {
		t.Errorf("idempotent replay: expected 2 entries, got %d", got)
	}
}

// TestAppendEntries_CommitIndex_AdvancesOnLeaderCommit verifies that the
// follower advances its commitIndex to min(leaderCommit, lastNewIndex).
func TestAppendEntries_CommitIndex_AdvancesOnLeaderCommit(t *testing.T) {
	node := raft.NewRaftNode("node-1", nil, nil)
	// Append 3 entries.
	node.HandleAppendEntries(aeArgs(1, "leader", 0, 0, []*pb.LogEntry{
		makeEntry(1, 1, "a"),
		makeEntry(2, 1, "b"),
		makeEntry(3, 1, "c"),
	}, 0))

	// Leader commits up to index 2.
	res := node.HandleAppendEntries(aeArgs(1, "leader", 3, 1, nil, 2))
	if !res.Success {
		t.Fatal("expected success")
	}
	if got := node.CommitIndex(); got != 2 {
		t.Errorf("expected commitIndex=2, got %d", got)
	}
}

// TestAppendEntries_CommitIndex_ClampedToLastNewIndex verifies that commitIndex
// does not exceed the last new entry index even if leaderCommit is higher.
func TestAppendEntries_CommitIndex_ClampedToLastNewIndex(t *testing.T) {
	node := raft.NewRaftNode("node-1", nil, nil)
	// Append only 2 entries; leader claims commit at 5.
	node.HandleAppendEntries(aeArgs(1, "leader", 0, 0, []*pb.LogEntry{
		makeEntry(1, 1, "a"),
		makeEntry(2, 1, "b"),
	}, 5))
	if got := node.CommitIndex(); got != 2 {
		t.Errorf("expected commitIndex clamped to 2 (lastNewIndex), got %d", got)
	}
}

// TestAppendEntries_StaleLeader_Rejected verifies that AppendEntries from a
// stale leader (lower term) is rejected, per §5.1.
func TestAppendEntries_StaleLeader_Rejected(t *testing.T) {
	node := raft.NewRaftNode("node-1", nil, nil)
	node.HandleAppendEntries(aeArgs(5, "leader-a", 0, 0, nil, 0)) // advance to term 5

	res := node.HandleAppendEntries(aeArgs(3, "leader-old", 0, 0, nil, 0))
	if res.Success {
		t.Error("expected rejection for stale-term leader")
	}
	if res.Term != 5 {
		t.Errorf("expected response term=5, got %d", res.Term)
	}
}
