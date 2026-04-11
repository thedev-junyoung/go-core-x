package grpc

import (
	"context"
	"fmt"

	"github.com/junyoung/core-x/internal/infrastructure/cluster"
	pb "github.com/junyoung/core-x/proto/pb"
)

// Forwarder forwards an ingest request to a peer node via gRPC.
type Forwarder struct {
	pool *ClientPool
}

// NewForwarder creates a Forwarder backed by the given client pool.
func NewForwarder(pool *ClientPool) *Forwarder {
	return &Forwarder{pool: pool}
}

// Forward sends an ingest request to the target node.
// Returns an error if the RPC fails or the remote node reports failure.
// The caller should map this error to HTTP 503.
func (f *Forwarder) Forward(ctx context.Context, node *cluster.Node, source string, payload []byte) error {
	client, err := f.pool.Get(node)
	if err != nil {
		return fmt.Errorf("forward get client: %w", err)
	}

	resp, err := client.Ingest(ctx, &pb.IngestRequest{
		Source:  source,
		Payload: payload,
	})
	if err != nil {
		return fmt.Errorf("forward rpc: %w", err)
	}
	if !resp.Ok {
		return fmt.Errorf("forward rejected: %s", resp.Message)
	}
	return nil
}

// ForwardGet forwards a KV Get request to a peer node.
func (f *Forwarder) ForwardGet(ctx context.Context, node *cluster.Node, key string) (*pb.GetResponse, error) {
	client, err := f.pool.GetKVClient(node)
	if err != nil {
		return nil, fmt.Errorf("forward get client: %w", err)
	}
	resp, err := client.Get(ctx, &pb.GetRequest{Key: key})
	if err != nil {
		return nil, fmt.Errorf("forward get rpc: %w", err)
	}
	return resp, nil
}
