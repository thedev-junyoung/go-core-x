package cluster

import (
	"context"
	"log/slog"
	"time"

	infraraft "github.com/junyoung/core-x/internal/infrastructure/raft"
)

// RaftRoleObserver is the minimal interface RoleController needs from RaftNode.
// Using a narrow interface instead of *infraraft.RaftNode keeps the controller
// testable without a full Raft node.
type RaftRoleObserver interface {
	Role() infraraft.RaftRole
	Term() int64
}

// ReplicationManager drives WAL streaming lifecycle based on the Raft role.
// It is the target of RoleController's transition callbacks.
type ReplicationManager interface {
	BecomeLeader(ctx context.Context) error
	BecomeFollower(ctx context.Context) error
	BecomeStandalone() error
}

// RoleController polls the Raft role at a fixed interval and calls the
// appropriate ReplicationManager method on each transition.
//
// Design: polling over callbacks (ADR-011).
// Callbacks would require RaftNode to hold a reference to the replication layer
// and call into it while holding mu — deadlock risk. Polling decouples the two
// layers at the cost of ≤50ms reaction latency, which is acceptable because
// HeartbeatInterval is already 50ms.
type RoleController struct {
	raft        RaftRoleObserver
	replication ReplicationManager
	interval    time.Duration
}

// NewRoleController creates a RoleController with the given poll interval.
// Pass 50*time.Millisecond for production use.
func NewRoleController(raft RaftRoleObserver, replication ReplicationManager, interval time.Duration) *RoleController {
	return &RoleController{
		raft:        raft,
		replication: replication,
		interval:    interval,
	}
}

// Run polls the Raft role at rc.interval and calls BecomeLeader / BecomeFollower
// on the ReplicationManager when the role changes. Blocks until ctx is cancelled.
// Call in a dedicated goroutine.
func (rc *RoleController) Run(ctx context.Context) {
	ticker := time.NewTicker(rc.interval)
	defer ticker.Stop()

	// Assume Follower as the initial state. The first poll may trigger an
	// immediate transition to Leader if this node wins an election before
	// the first tick — that is intentional and correct.
	current := infraraft.RoleFollower

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			next := rc.raft.Role()
			if next == current {
				continue
			}

			term := rc.raft.Term()
			slog.Info("role_controller: raft role changed",
				"from", current.String(),
				"to", next.String(),
				"term", term,
			)

			var err error
			switch next {
			case infraraft.RoleLeader:
				err = rc.replication.BecomeLeader(ctx)
			default:
				// Follower and Candidate both deactivate replication streaming.
				err = rc.replication.BecomeFollower(ctx)
			}
			if err != nil {
				slog.Error("role_controller: replication transition failed",
					"role", next.String(),
					"err", err,
				)
				// Do not update current: retry on next tick.
				continue
			}

			current = next
		}
	}
}
