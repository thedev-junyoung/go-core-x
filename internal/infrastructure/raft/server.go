package raft

import (
	"context"

	pb "github.com/junyoung/core-x/proto/pb"
)

// RaftHandler is the interface RaftServer calls into on incoming RPCs.
// Separating this interface from RaftNode allows clean testing with fakes.
type RaftHandler interface {
	HandleAppendEntries(term int64, leaderID string) (currentTerm int64, success bool)
	HandleRequestVote(term int64, candidateID string, lastLogIndex, lastLogTerm int64) (currentTerm int64, voteGranted bool)
}

// RaftServer implements pb.RaftServiceServer.
// It delegates all state decisions to the RaftHandler (normally *RaftNode).
type RaftServer struct {
	pb.UnimplementedRaftServiceServer
	node RaftHandler
}

// NewRaftServer creates a RaftServer backed by the given handler.
func NewRaftServer(node RaftHandler) *RaftServer {
	return &RaftServer{node: node}
}

// AppendEntries handles incoming heartbeat RPCs from the Leader.
func (s *RaftServer) AppendEntries(_ context.Context, req *pb.AppendEntriesRequest) (*pb.AppendEntriesResponse, error) {
	term, success := s.node.HandleAppendEntries(req.Term, req.LeaderId)
	return &pb.AppendEntriesResponse{Term: term, Success: success}, nil
}

// RequestVote handles incoming vote requests from Candidates.
func (s *RaftServer) RequestVote(_ context.Context, req *pb.RequestVoteRequest) (*pb.RequestVoteResponse, error) {
	term, granted := s.node.HandleRequestVote(req.Term, req.CandidateId, req.LastLogIndex, req.LastLogTerm)
	return &pb.RequestVoteResponse{Term: term, VoteGranted: granted}, nil
}
