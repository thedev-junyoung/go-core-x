// Package raft implements Phase 5a Raft leader election.
// Phase 5b will extend this with log replication.
package raft

import "time"

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
const (
	ElectionTimeoutMin = 150 * time.Millisecond
	ElectionTimeoutMax = 300 * time.Millisecond
	HeartbeatInterval  = 50 * time.Millisecond
)
