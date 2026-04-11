package raft_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/junyoung/core-x/internal/infrastructure/raft"
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

	term, success := node.HandleAppendEntries(5, "node-2")
	if !success {
		t.Error("expected success=true")
	}
	if term != 5 {
		t.Errorf("expected returned term=5, got %d", term)
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
	node.HandleAppendEntries(10, "node-2") // advance term to 10

	term, success := node.HandleAppendEntries(5, "node-3") // stale leader
	if success {
		t.Error("expected success=false for stale term")
	}
	if term != 10 {
		t.Errorf("expected current term 10 in response, got %d", term)
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
	_, success := node.HandleAppendEntries(4, "node-3")
	if !success {
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

	node.HandleAppendEntries(7, "leader-1")

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
