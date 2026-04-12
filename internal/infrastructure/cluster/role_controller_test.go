package cluster_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/junyoung/core-x/internal/infrastructure/cluster"
	infraraft "github.com/junyoung/core-x/internal/infrastructure/raft"
)

// --- fakes -------------------------------------------------------------------

// fakeRaftObserver simulates RaftNode.Role() / Term() for controller tests.
type fakeRaftObserver struct {
	role atomic.Int32
	term atomic.Int64
}

func (f *fakeRaftObserver) Role() infraraft.RaftRole {
	return infraraft.RaftRole(f.role.Load())
}

func (f *fakeRaftObserver) Term() int64 {
	return f.term.Load()
}

func (f *fakeRaftObserver) setRole(r infraraft.RaftRole) { f.role.Store(int32(r)) }
func (f *fakeRaftObserver) setTerm(t int64)              { f.term.Store(t) }

// fakeReplicationManager records BecomeLeader / BecomeFollower calls.
type fakeReplicationManager struct {
	leaderCalls   atomic.Int32
	followerCalls atomic.Int32
	lastErr       error
}

func (f *fakeReplicationManager) BecomeLeader(_ context.Context) error {
	f.leaderCalls.Add(1)
	return f.lastErr
}

func (f *fakeReplicationManager) BecomeFollower(_ context.Context) error {
	f.followerCalls.Add(1)
	return f.lastErr
}

func (f *fakeReplicationManager) BecomeStandalone() error { return nil }

// --- helpers -----------------------------------------------------------------

func waitFor(t *testing.T, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", desc)
}

// --- tests -------------------------------------------------------------------

// TestRoleController_FollowerToLeader verifies that a Follower→Leader transition
// triggers exactly one BecomeLeader call.
func TestRoleController_FollowerToLeader(t *testing.T) {
	t.Parallel()

	raft := &fakeRaftObserver{}
	raft.setRole(infraraft.RoleFollower)

	repl := &fakeReplicationManager{}
	rc := cluster.NewRoleController(raft, repl, 10*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rc.Run(ctx)

	// Transition to Leader.
	raft.setRole(infraraft.RoleLeader)
	raft.setTerm(2)

	waitFor(t, "BecomeLeader called", func() bool {
		return repl.leaderCalls.Load() >= 1
	})

	if n := repl.followerCalls.Load(); n != 0 {
		t.Errorf("BecomeFollower called %d times; want 0", n)
	}
}

// TestRoleController_LeaderToFollower verifies that a Leader→Follower transition
// triggers BecomeFollower after BecomeLeader.
func TestRoleController_LeaderToFollower(t *testing.T) {
	t.Parallel()

	raft := &fakeRaftObserver{}
	raft.setRole(infraraft.RoleFollower)

	repl := &fakeReplicationManager{}
	rc := cluster.NewRoleController(raft, repl, 10*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rc.Run(ctx)

	// Step 1: become leader.
	raft.setRole(infraraft.RoleLeader)
	waitFor(t, "BecomeLeader called", func() bool {
		return repl.leaderCalls.Load() >= 1
	})

	// Step 2: step down.
	raft.setRole(infraraft.RoleFollower)
	waitFor(t, "BecomeFollower called", func() bool {
		return repl.followerCalls.Load() >= 1
	})
}

// TestRoleController_NoCallOnSameRole verifies that repeated polls without a
// role change do not cause redundant manager calls.
func TestRoleController_NoCallOnSameRole(t *testing.T) {
	t.Parallel()

	raft := &fakeRaftObserver{}
	raft.setRole(infraraft.RoleFollower)

	repl := &fakeReplicationManager{}
	rc := cluster.NewRoleController(raft, repl, 10*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	go rc.Run(ctx)

	// Let the controller poll several times without a role change.
	time.Sleep(60 * time.Millisecond)
	cancel()

	// Role never changed from Follower → no manager calls expected.
	if n := repl.leaderCalls.Load(); n != 0 {
		t.Errorf("BecomeLeader called %d times; want 0", n)
	}
	if n := repl.followerCalls.Load(); n != 0 {
		t.Errorf("BecomeFollower called %d times; want 0", n)
	}
}

// TestRoleController_CandidateTriggersFollower verifies that the Candidate role
// is treated equivalently to Follower (streaming deactivated).
func TestRoleController_CandidateTriggersFollower(t *testing.T) {
	t.Parallel()

	raft := &fakeRaftObserver{}
	raft.setRole(infraraft.RoleFollower)

	repl := &fakeReplicationManager{}
	rc := cluster.NewRoleController(raft, repl, 10*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rc.Run(ctx)

	// Transition to Leader first so there is a prior state to compare against.
	raft.setRole(infraraft.RoleLeader)
	waitFor(t, "BecomeLeader called", func() bool {
		return repl.leaderCalls.Load() >= 1
	})

	// Transition to Candidate: should call BecomeFollower (not BecomeLeader).
	raft.setRole(infraraft.RoleCandidate)
	waitFor(t, "BecomeFollower called after Candidate transition", func() bool {
		return repl.followerCalls.Load() >= 1
	})

	if n := repl.leaderCalls.Load(); n != 1 {
		t.Errorf("BecomeLeader called %d times; want exactly 1", n)
	}
}

// TestRoleController_ContextCancellation verifies Run exits promptly when ctx
// is cancelled.
func TestRoleController_ContextCancellation(t *testing.T) {
	t.Parallel()

	raft := &fakeRaftObserver{}
	raft.setRole(infraraft.RoleFollower)

	repl := &fakeReplicationManager{}
	rc := cluster.NewRoleController(raft, repl, 10*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		rc.Run(ctx)
		close(done)
	}()

	cancel()

	select {
	case <-done:
		// OK
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Run did not exit after context cancellation within 500ms")
	}
}
