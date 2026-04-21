package raft

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/junyoung/core-x/proto/pb"
)

const (
	rpcTimeout           = 100 * time.Millisecond
	snapshotChunkSize    = 1 * 1024 * 1024 // 1 MB per chunk
	snapshotSendTimeout  = 30 * time.Second // generous timeout for large snapshots
)

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

// InstallSnapshot sends snapshot data to the peer as a client-streaming RPC (§7).
// data is serialised with marshalSnapshotData and split into snapshotChunkSize chunks.
// Returns the peer's current term (for leader step-down detection) on success.
func (c *RaftClient) InstallSnapshot(
	term int64,
	leaderID string,
	meta SnapshotMeta,
	data SnapshotData,
) (int64, error) {
	raw, err := marshalSnapshotData(meta.Index, meta.Term, data)
	if err != nil {
		return 0, fmt.Errorf("raft: install snapshot: marshal: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), snapshotSendTimeout)
	defer cancel()

	stream, err := c.rpc.InstallSnapshot(ctx)
	if err != nil {
		return 0, fmt.Errorf("raft: install snapshot: open stream: %w", err)
	}

	offset := int64(0)
	total := int64(len(raw))
	for offset < total {
		end := offset + snapshotChunkSize
		if end > total {
			end = total
		}
		chunk := raw[offset:end]
		done := end == total

		req := &pb.InstallSnapshotRequest{
			Term:              term,
			LeaderId:          leaderID,
			LastIncludedIndex: meta.Index,
			LastIncludedTerm:  meta.Term,
			Offset:            offset,
			Data:              chunk,
			Done:              done,
		}
		if err := stream.Send(req); err != nil {
			return 0, fmt.Errorf("raft: install snapshot: send chunk at offset %d: %w", offset, err)
		}
		offset = end
	}

	resp, err := stream.CloseAndRecv()
	if err != nil {
		return 0, fmt.Errorf("raft: install snapshot: close stream: %w", err)
	}
	return resp.Term, nil
}

// Close releases the gRPC connection.
func (c *RaftClient) Close() error {
	return c.conn.Close()
}

// PeerClients manages RaftClient instances for all peers.
// The clients map is guarded by mu to allow EnsureConnected to add peers safely
// after construction (ADR-020 §7: dynamic membership changes).
type PeerClients struct {
	mu      sync.RWMutex
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

// All returns a snapshot of all peer clients. Safe for concurrent use.
func (p *PeerClients) All() []*RaftClient {
	p.mu.RLock()
	defer p.mu.RUnlock()
	result := make([]*RaftClient, 0, len(p.clients))
	for _, c := range p.clients {
		result = append(result, c)
	}
	return result
}

// EnsureConnected dials any addresses in addrs that are not yet in p.clients.
// Idempotent: addresses already present are skipped. Safe for concurrent use.
// Called by applyConfigEntry to connect to newly-joining peers (ADR-020 §7).
func (p *PeerClients) EnsureConnected(addrs []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, addr := range addrs {
		if _, ok := p.clients[addr]; ok {
			continue // already connected
		}
		c, err := NewRaftClient(addr)
		if err != nil {
			slog.Warn("raft: EnsureConnected: failed to dial new peer",
				"addr", addr, "err", err)
			continue
		}
		p.clients[addr] = c
		slog.Info("raft: EnsureConnected: dialed new peer", "addr", addr)
	}
}

// InstallSnapshot sends a snapshot to the named peer (keyed by addr).
// Returns an error if the peer is not found or the transfer fails.
// On success, if the peer's term exceeds our term the caller should step down.
func (p *PeerClients) InstallSnapshot(
	peerID string,
	term int64,
	leaderID string,
	meta SnapshotMeta,
	data SnapshotData,
) error {
	p.mu.RLock()
	c, ok := p.clients[peerID]
	p.mu.RUnlock()
	if !ok {
		return fmt.Errorf("raft: install snapshot: unknown peer %q", peerID)
	}
	_, err := c.InstallSnapshot(term, leaderID, meta, data)
	return err
}

// Close closes all peer connections.
func (p *PeerClients) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.clients {
		c.Close()
	}
}
