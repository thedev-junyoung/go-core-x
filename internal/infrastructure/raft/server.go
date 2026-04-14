package raft

import (
	"context"

	pb "github.com/junyoung/core-x/proto/pb"
)

// AppendEntriesArgs carries the decoded fields from an AppendEntriesRequest.
// Using a struct avoids a long parameter list and makes future extensions cheaper.
type AppendEntriesArgs struct {
	Term         int64
	LeaderID     string
	PrevLogIndex int64
	PrevLogTerm  int64
	Entries      []*pb.LogEntry
	LeaderCommit int64
}

// AppendEntriesResult carries the decoded fields for an AppendEntriesResponse.
type AppendEntriesResult struct {
	Term          int64
	Success       bool
	ConflictIndex int64 // Fast Backup: first index of conflicting term
	ConflictTerm  int64 // Fast Backup: term at prevLogIndex (0 = no entry)
}

// InstallSnapshotArgs carries a single chunk of a snapshot transfer (§7).
type InstallSnapshotArgs struct {
	Term              int64
	LeaderID          string
	LastIncludedIndex int64
	LastIncludedTerm  int64
	Offset            int64
	Data              []byte
	Done              bool
}

// InstallSnapshotResult is the follower's reply to a snapshot chunk.
type InstallSnapshotResult struct {
	Term int64
}

// RaftHandler is the interface RaftServer calls into on incoming RPCs.
// Separating this interface from RaftNode allows clean testing with fakes.
type RaftHandler interface {
	HandleAppendEntries(args AppendEntriesArgs) AppendEntriesResult
	HandleRequestVote(term int64, candidateID string, lastLogIndex, lastLogTerm int64) (currentTerm int64, voteGranted bool)
	HandleInstallSnapshot(req InstallSnapshotArgs) InstallSnapshotResult
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

// AppendEntries handles incoming heartbeat / log-replication RPCs from the Leader.
func (s *RaftServer) AppendEntries(_ context.Context, req *pb.AppendEntriesRequest) (*pb.AppendEntriesResponse, error) {
	res := s.node.HandleAppendEntries(AppendEntriesArgs{
		Term:         req.Term,
		LeaderID:     req.LeaderId,
		PrevLogIndex: req.PrevLogIndex,
		PrevLogTerm:  req.PrevLogTerm,
		Entries:      req.Entries,
		LeaderCommit: req.LeaderCommit,
	})
	return &pb.AppendEntriesResponse{
		Term:          res.Term,
		Success:       res.Success,
		ConflictIndex: res.ConflictIndex,
		ConflictTerm:  res.ConflictTerm,
	}, nil
}

// RequestVote handles incoming vote requests from Candidates.
func (s *RaftServer) RequestVote(_ context.Context, req *pb.RequestVoteRequest) (*pb.RequestVoteResponse, error) {
	term, granted := s.node.HandleRequestVote(req.Term, req.CandidateId, req.LastLogIndex, req.LastLogTerm)
	return &pb.RequestVoteResponse{Term: term, VoteGranted: granted}, nil
}

// InstallSnapshot handles an incoming client-streaming InstallSnapshot RPC (§7).
// The server receives chunks in order; on the final chunk (done=true) it returns
// the follower's current term so the leader can detect a stale leadership.
func (s *RaftServer) InstallSnapshot(stream pb.RaftService_InstallSnapshotServer) error {
	var lastResult InstallSnapshotResult
	for {
		chunk, err := stream.Recv()
		if err != nil {
			return err
		}

		args := InstallSnapshotArgs{
			Term:              chunk.Term,
			LeaderID:          chunk.LeaderId,
			LastIncludedIndex: chunk.LastIncludedIndex,
			LastIncludedTerm:  chunk.LastIncludedTerm,
			Offset:            chunk.Offset,
			Data:              chunk.Data,
			Done:              chunk.Done,
		}
		lastResult = s.node.HandleInstallSnapshot(args)

		if chunk.Done {
			break
		}
	}

	return stream.SendAndClose(&pb.InstallSnapshotResponse{
		Term: lastResult.Term,
	})
}
