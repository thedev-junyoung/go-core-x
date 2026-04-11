// Package grpc implements the gRPC transport layer for inter-node communication.
//
// Design:
//   - GRPCIngestionServer receives forwarded Ingest requests from peer nodes.
//   - It delegates directly to IngestionService — the same Application layer
//     used by the HTTP handler. No business logic lives here.
//   - This is the Phase 3 Extension Point defined in README: "Phase 3: gRPC
//     ingestion → infrastructure/grpc/handler.go 신규 추가 (동일 IngestionService 재사용)"
package grpc

import (
	"context"
	"log/slog"
	"net"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	appingestion "github.com/junyoung/core-x/internal/application/ingestion"
	pb "github.com/junyoung/core-x/proto/pb"
)

// GRPCIngestionServer implements the gRPC IngestionService.
// It wraps IngestionService so that inter-node traffic follows the same
// ingestion path as HTTP traffic.
type GRPCIngestionServer struct {
	svc *appingestion.IngestionService
	pb.UnimplementedIngestionServiceServer
}

// NewGRPCIngestionServer creates a server backed by the given IngestionService.
func NewGRPCIngestionServer(svc *appingestion.IngestionService) *GRPCIngestionServer {
	return &GRPCIngestionServer{svc: svc}
}

// Ingest handles a forwarded ingest request from a peer node.
func (s *GRPCIngestionServer) Ingest(_ context.Context, req *pb.IngestRequest) (*pb.IngestResponse, error) {
	if err := s.svc.Ingest(req.Source, string(req.Payload)); err != nil {
		slog.Warn("grpc ingest failed", "source", req.Source, "err", err)
		return &pb.IngestResponse{Ok: false, Message: err.Error()}, nil
	}
	return &pb.IngestResponse{Ok: true}, nil
}

// Server wraps a gRPC server instance with its listener for lifecycle management.
type Server struct {
	grpcServer *grpc.Server
	listener   net.Listener
	Addr       string
}

// NewServer creates and registers a gRPC server on the given address.
func NewServer(addr string, svc *appingestion.IngestionService) (*Server, error) {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}

	gs := grpc.NewServer(
		grpc.Creds(insecure.NewCredentials()),
	)
	pb.RegisterIngestionServiceServer(gs, NewGRPCIngestionServer(svc))

	return &Server{
		grpcServer: gs,
		listener:   lis,
		Addr:       lis.Addr().String(),
	}, nil
}

// Serve starts accepting connections. Blocks until Stop is called.
func (s *Server) Serve() error {
	return s.grpcServer.Serve(s.listener)
}

// Stop performs a graceful shutdown.
func (s *Server) Stop() {
	s.grpcServer.GracefulStop()
}

// RegisterReplicationService registers a ReplicationService implementation
// with the underlying gRPC server. Must be called before Serve().
func (s *Server) RegisterReplicationService(srv pb.ReplicationServiceServer) {
	pb.RegisterReplicationServiceServer(s.grpcServer, srv)
}
