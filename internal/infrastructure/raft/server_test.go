package raft_test

import (
	"context"
	"testing"

	"github.com/junyoung/core-x/internal/infrastructure/raft"
	pb "github.com/junyoung/core-x/proto/pb"
)

// fakeNode simulates the subset of RaftNode the server needs.
type fakeNode struct {
	handleAppendEntriesCalled bool
	handleRequestVoteCalled   bool
	grantVote                 bool
	currentTerm               int64
}

func (f *fakeNode) HandleAppendEntries(args raft.AppendEntriesArgs) raft.AppendEntriesResult {
	f.handleAppendEntriesCalled = true
	return raft.AppendEntriesResult{Term: f.currentTerm, Success: true}
}

func (f *fakeNode) HandleRequestVote(term int64, candidateID string, lastLogIndex, lastLogTerm int64) (currentTerm int64, voteGranted bool) {
	f.handleRequestVoteCalled = true
	return f.currentTerm, f.grantVote
}

func TestRaftServer_AppendEntries(t *testing.T) {
	node := &fakeNode{currentTerm: 1}
	srv := raft.NewRaftServer(node)

	resp, err := srv.AppendEntries(context.Background(), &pb.AppendEntriesRequest{
		Term:     1,
		LeaderId: "node-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Success {
		t.Error("expected success=true")
	}
	if !node.handleAppendEntriesCalled {
		t.Error("expected HandleAppendEntries to be called")
	}
}

func TestRaftServer_RequestVote_Granted(t *testing.T) {
	node := &fakeNode{currentTerm: 1, grantVote: true}
	srv := raft.NewRaftServer(node)

	resp, err := srv.RequestVote(context.Background(), &pb.RequestVoteRequest{
		Term:        2,
		CandidateId: "node-2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.VoteGranted {
		t.Error("expected vote_granted=true")
	}
}
