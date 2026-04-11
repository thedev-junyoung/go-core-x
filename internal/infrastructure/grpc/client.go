package grpc

import (
	"fmt"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/junyoung/core-x/internal/infrastructure/cluster"
	pb "github.com/junyoung/core-x/proto/pb"
)

// ClientPool maintains one gRPC ClientConn per peer node.
// Connections are created lazily on first use and reused thereafter.
//
// sync.Map is used because reads (Get) vastly outnumber writes (new node
// registration or removal), and sync.Map is optimised for that pattern.
type ClientPool struct {
	mu    sync.Mutex
	conns map[string]*grpc.ClientConn // nodeID → conn
}

// NewClientPool creates an empty pool.
func NewClientPool() *ClientPool {
	return &ClientPool{conns: make(map[string]*grpc.ClientConn)}
}

// conn returns the underlying *grpc.ClientConn for node, creating it if needed.
func (p *ClientPool) conn(node *cluster.Node) (*grpc.ClientConn, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.conns[node.ID]
	if !ok {
		var err error
		c, err = grpc.NewClient(
			node.Addr,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
		if err != nil {
			return nil, fmt.Errorf("grpc dial %s: %w", node.Addr, err)
		}
		p.conns[node.ID] = c
	}
	return c, nil
}

// Get returns an IngestionServiceClient for the given node, creating the
// underlying connection if it does not exist yet.
func (p *ClientPool) Get(node *cluster.Node) (pb.IngestionServiceClient, error) {
	c, err := p.conn(node)
	if err != nil {
		return nil, err
	}
	return pb.NewIngestionServiceClient(c), nil
}

// GetKVClient returns a KVServiceClient for the given node, creating the
// underlying connection if it does not exist yet.
func (p *ClientPool) GetKVClient(node *cluster.Node) (pb.KVServiceClient, error) {
	c, err := p.conn(node)
	if err != nil {
		return nil, err
	}
	return pb.NewKVServiceClient(c), nil
}

// Remove closes and removes the connection for a node.
// Safe to call when a node leaves the cluster.
func (p *ClientPool) Remove(nodeID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if conn, ok := p.conns[nodeID]; ok {
		_ = conn.Close()
		delete(p.conns, nodeID)
	}
}

// Close shuts down all pooled connections.
func (p *ClientPool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, conn := range p.conns {
		_ = conn.Close()
		delete(p.conns, id)
	}
}
