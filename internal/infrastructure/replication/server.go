package replication

import (
	"log/slog"

	pb "github.com/junyoung/core-x/proto/pb"
)

// ReplicationServer implements the gRPC ReplicationService.
type ReplicationServer struct {
	pb.UnimplementedReplicationServiceServer
	streamer *Streamer
}

// NewReplicationServer creates a ReplicationServer backed by the given Streamer.
func NewReplicationServer(streamer *Streamer) *ReplicationServer {
	return &ReplicationServer{streamer: streamer}
}

// StreamWAL handles a replica's request to stream WAL entries.
func (s *ReplicationServer) StreamWAL(req *pb.StreamWALRequest, stream pb.ReplicationService_StreamWALServer) error {
	slog.Info("replication: replica connected",
		"replica_id", req.ReplicaId,
		"start_offset", req.StartOffset,
	)

	err := s.streamer.Stream(stream.Context(), req.StartOffset, func(entry RawEntry) error {
		return stream.Send(&pb.WALEntry{
			Offset:     entry.Offset,
			RawData:    entry.RawData,
			RecordSize: entry.RecordSize,
		})
	})

	if err != nil && stream.Context().Err() == nil {
		slog.Warn("replication: streamer error", "replica_id", req.ReplicaId, "err", err)
	}

	slog.Info("replication: replica disconnected", "replica_id", req.ReplicaId)
	return err
}
