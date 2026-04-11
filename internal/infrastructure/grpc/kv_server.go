package grpc

import (
	"context"
	"errors"

	"github.com/junyoung/core-x/internal/domain"
	kvstore "github.com/junyoung/core-x/internal/infrastructure/storage/kv"
	pb "github.com/junyoung/core-x/proto/pb"
)

// KVGetter is the port for KV point-lookups.
// kv.Store implements this interface.
type KVGetter interface {
	Get(key string) (*domain.Event, error)
}

// GRPCKVServer handles Get RPCs from peer nodes.
type GRPCKVServer struct {
	kv KVGetter
	pb.UnimplementedKVServiceServer
}

// NewGRPCKVServer creates a server backed by the given KVGetter.
func NewGRPCKVServer(kv KVGetter) *GRPCKVServer {
	return &GRPCKVServer{kv: kv}
}

// Get handles a KV lookup RPC.
func (s *GRPCKVServer) Get(_ context.Context, req *pb.GetRequest) (*pb.GetResponse, error) {
	event, err := s.kv.Get(req.Key)
	if err != nil {
		if errors.Is(err, kvstore.ErrKeyNotFound) {
			return &pb.GetResponse{Found: false}, nil
		}
		return nil, err
	}
	return &pb.GetResponse{
		Found:            true,
		Payload:          []byte(event.Payload),
		Source:           event.Source,
		ReceivedAtUnixNs: event.ReceivedAt.UnixNano(),
	}, nil
}
