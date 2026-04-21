// Package raft implements the Raft consensus algorithm (leader election,
// log replication, and write path).
package raft

import (
	"errors"
	"time"
)

// Sentinel errors for Raft operations.
var (
	// ErrNotLeader is returned by ReadIndex when the node is not the current leader.
	ErrNotLeader = errors.New("raft: not leader")

	// ErrReadIndexTimeout is returned by ReadIndex when the quorum heartbeat
	// confirmation round does not complete before the context deadline.
	ErrReadIndexTimeout = errors.New("raft: read index confirmation timed out")

	// ErrConfigChangeInProgress is returned by ProposeConfigChange when a
	// membership change is already underway (cluster is in joint phase).
	// Callers must wait for the current change to complete before retrying.
	ErrConfigChangeInProgress = errors.New("raft: config change already in progress")
)

// RaftRole represents the current Raft state of a node.
type RaftRole int32

const (
	// RoleFollower is the initial role. Follower expects heartbeats from a Leader.
	// On election timeout it transitions to Candidate.
	RoleFollower RaftRole = iota
	// RoleCandidate is active during an election. Candidate sends RequestVote to peers.
	RoleCandidate
	// RoleLeader sends periodic heartbeats (AppendEntries) to all peers.
	RoleLeader
)

// String returns a human-readable role name for logging.
func (r RaftRole) String() string {
	switch r {
	case RoleFollower:
		return "follower"
	case RoleCandidate:
		return "candidate"
	case RoleLeader:
		return "leader"
	default:
		return "unknown"
	}
}

// Raft timing constants.
// ElectionTimeoutMin / Max: Raft paper recommends 150–300ms.
// HeartbeatInterval must be << ElectionTimeoutMin to prevent spurious elections.
// applyPollInterval: how often runApplyLoop checks commitIndex > lastApplied.
// Kept well below HeartbeatInterval so apply latency is dominated by commit
// latency, not polling lag.
const (
	ElectionTimeoutMin = 150 * time.Millisecond
	ElectionTimeoutMax = 300 * time.Millisecond
	HeartbeatInterval  = 50 * time.Millisecond
	applyPollInterval  = 5 * time.Millisecond

	// leaseDuration is the validity window granted to a leader lease after a
	// quorum of heartbeat acknowledgments. Must be < ElectionTimeoutMin to
	// guarantee that a stale leader's lease expires before a new leader is elected.
	leaseDuration = 130 * time.Millisecond
)
