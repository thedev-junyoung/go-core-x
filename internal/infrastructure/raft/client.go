package raft

import (
	"context"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/junyoung/core-x/proto/pb"
)

const rpcTimeout = 100 * time.Millisecond

// RaftClient sends Raft RPCs to a single peer.
type RaftClient struct {
	addr string
	conn *grpc.ClientConn
	rpc  pb.RaftServiceClient
}

// NewRaftClient dials the peer at addr and returns a ready-to-use client.
// The caller must call Close when done.
func NewRaftClient(addr string) (*RaftClient, error) {
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, err
	}
	return &RaftClient{
		addr: addr,
		conn: conn,
		rpc:  pb.NewRaftServiceClient(conn),
	}, nil
}

// RequestVote sends a RequestVote RPC and returns the response.
// Returns nil, err on timeout or network failure.
func (c *RaftClient) RequestVote(term int64, candidateID string, lastLogIndex, lastLogTerm int64) (*pb.RequestVoteResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	return c.rpc.RequestVote(ctx, &pb.RequestVoteRequest{
		Term:         term,
		CandidateId:  candidateID,
		LastLogIndex: lastLogIndex,
		LastLogTerm:  lastLogTerm,
	})
}

// AppendEntries sends an AppendEntries RPC (heartbeat or log replication).
// Returns nil, err on timeout or network failure.
func (c *RaftClient) AppendEntries(args AppendEntriesArgs) (*pb.AppendEntriesResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	return c.rpc.AppendEntries(ctx, &pb.AppendEntriesRequest{
		Term:         args.Term,
		LeaderId:     args.LeaderID,
		PrevLogIndex: args.PrevLogIndex,
		PrevLogTerm:  args.PrevLogTerm,
		Entries:      args.Entries,
		LeaderCommit: args.LeaderCommit,
	})
}

// Close releases the gRPC connection.
func (c *RaftClient) Close() error {
	return c.conn.Close()
}

// PeerClients manages RaftClient instances for all peers.
type PeerClients struct {
	clients map[string]*RaftClient // addr → client
}

// NewPeerClients dials all peer addresses and returns a PeerClients.
// Closes all connections if any dial fails.
func NewPeerClients(addrs []string) (*PeerClients, error) {
	clients := make(map[string]*RaftClient, len(addrs))
	for _, addr := range addrs {
		c, err := NewRaftClient(addr)
		if err != nil {
			for _, existing := range clients {
				existing.Close()
			}
			return nil, err
		}
		clients[addr] = c
	}
	return &PeerClients{clients: clients}, nil
}

// All returns all peer clients.
func (p *PeerClients) All() []*RaftClient {
	result := make([]*RaftClient, 0, len(p.clients))
	for _, c := range p.clients {
		result = append(result, c)
	}
	return result
}

// Close closes all peer connections.
func (p *PeerClients) Close() {
	for _, c := range p.clients {
		c.Close()
	}
}
