package raft

import (
	"context"
	"log/slog"
	"math/rand"
	"sync"
	"time"
)

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

	// lastLogIndex and lastLogTerm track the last entry in this node's log.
	// Used for §5.4.1 election restriction. Both are 0 until Phase 5b adds
	// actual log entries via HandleAppendEntries.
	lastLogIndex int64
	lastLogTerm  int64

	// resetCh is sent to from Handle* methods to reset the election timer.
	// Buffer of 1: sender never blocks.
	resetCh chan struct{}
}

// NewRaftNode creates a RaftNode. peers may be nil for single-node or test use.
func NewRaftNode(id string, peers *PeerClients) *RaftNode {
	return &RaftNode{
		id:      id,
		peers:   peers,
		role:    RoleFollower,
		resetCh: make(chan struct{}, 1),
	}
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

// ForceLog sets lastLogIndex and lastLogTerm directly. Used only in tests
// to simulate a node that has already applied log entries.
func (n *RaftNode) ForceLog(lastLogIndex, lastLogTerm int64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.lastLogIndex = lastLogIndex
	n.lastLogTerm = lastLogTerm
}

// HandleAppendEntries processes an incoming AppendEntries (heartbeat) RPC.
// Implements RaftHandler.
func (n *RaftNode) HandleAppendEntries(term int64, leaderID string) (currentTerm int64, success bool) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if term < n.currentTerm {
		// Stale leader — reject.
		return n.currentTerm, false
	}

	// Valid heartbeat: update term, revert to Follower.
	if term > n.currentTerm {
		n.currentTerm = term
		n.votedFor = ""
	}
	n.role = RoleFollower

	// Signal the run loop to reset the election timer (non-blocking).
	select {
	case n.resetCh <- struct{}{}:
	default:
	}

	return n.currentTerm, true
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
	}

	// §5.4.1: deny vote if candidate's log is behind ours.
	if !n.candidateLogUpToDate(lastLogIndex, lastLogTerm) {
		return n.currentTerm, false
	}

	// Grant vote if we haven't voted for anyone else this term.
	if n.votedFor == "" || n.votedFor == candidateID {
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
// This is the invariant that guarantees a newly elected leader always holds
// all committed entries — because any committed entry was replicated to a
// majority, and a candidate needs votes from a majority, so at least one
// voter in that majority has the entry and will deny votes to any candidate
// that is missing it.
func (n *RaftNode) candidateLogUpToDate(candidateLastLogIndex, candidateLastLogTerm int64) bool {
	if candidateLastLogTerm != n.lastLogTerm {
		return candidateLastLogTerm > n.lastLogTerm
	}
	return candidateLastLogIndex >= n.lastLogIndex
}

// Run starts the Raft state machine. Blocks until ctx is cancelled.
// Call this in a dedicated goroutine.
func (n *RaftNode) Run(ctx context.Context) {
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
	n.votedFor = n.id
	n.role = RoleCandidate
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
			resp, err := peer.RequestVote(term, n.id, 0, 0)
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
//  1. Send AppendEntries (heartbeat) to all peers every HeartbeatInterval.
//  2. If any peer responds with higher term → step down to Follower.
//  3. Exits when ctx is cancelled or role changes externally (HandleAppendEntries from higher-term leader).
func (n *RaftNode) runLeader(ctx context.Context) {
	n.mu.Lock()
	term := n.currentTerm
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
		}
	}
}

// sendHeartbeats sends AppendEntries to all peers concurrently.
// If any peer returns a higher term, this node steps down.
func (n *RaftNode) sendHeartbeats(ctx context.Context, peers []*RaftClient, term int64) {
	if len(peers) == 0 {
		return
	}

	type result struct {
		respTerm int64
		err      error
	}
	ch := make(chan result, len(peers))

	for _, peer := range peers {
		peer := peer
		go func() {
			resp, err := peer.AppendEntries(term, n.id)
			if err != nil {
				ch <- result{err: err}
				return
			}
			ch <- result{respTerm: resp.Term}
		}()
	}

	for range peers {
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
		}
	}
}
