package grpc_test

import (
	"context"
	"testing"
	"time"

	"github.com/junyoung/core-x/internal/domain"
	infragrpc "github.com/junyoung/core-x/internal/infrastructure/grpc"
	kvstore "github.com/junyoung/core-x/internal/infrastructure/storage/kv"
	pb "github.com/junyoung/core-x/proto/pb"
)

type stubKVGetter struct {
	event *domain.Event
	err   error
}

func (s *stubKVGetter) Get(key string) (*domain.Event, error) {
	return s.event, s.err
}

func TestGRPCKVServer_Get_Found(t *testing.T) {
	stub := &stubKVGetter{
		event: &domain.Event{Source: "src1", Payload: "hello", ReceivedAt: time.Unix(0, 1234567890)},
	}
	srv := infragrpc.NewGRPCKVServer(stub)
	resp, err := srv.Get(context.Background(), &pb.GetRequest{Key: "src1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.Found {
		t.Fatal("expected found=true")
	}
	if string(resp.Payload) != "hello" {
		t.Fatalf("expected payload=hello, got %s", resp.Payload)
	}
	if resp.Source != "src1" {
		t.Fatalf("expected source=src1, got %s", resp.Source)
	}
}

func TestGRPCKVServer_Get_NotFound(t *testing.T) {
	stub := &stubKVGetter{err: kvstore.ErrKeyNotFound}
	srv := infragrpc.NewGRPCKVServer(stub)
	resp, err := srv.Get(context.Background(), &pb.GetRequest{Key: "missing"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Found {
		t.Fatal("expected found=false")
	}
}
