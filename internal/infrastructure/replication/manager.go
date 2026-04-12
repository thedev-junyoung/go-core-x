package replication

import (
	"context"
	"log/slog"
	"sync/atomic"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/junyoung/core-x/proto/pb"
)

// ReplicationManager controls the lifecycle of WAL streaming based on the
// current Raft role. It exposes BecomeLeader / BecomeFollower so that a
// RoleController can drive role transitions without coupling RaftNode to the
// replication layer.
//
// gRPC constraint: grpc.Server does not support handler re-registration after
// Serve() begins. The solution is the null-object pattern: ReplicationServer is
// always registered; an atomic flag (active) gates whether StreamWAL actually
// processes requests. Inactive calls return codes.Unavailable immediately.
type ReplicationManager struct {
	// streamerFactory creates a new Streamer on demand. Injected at construction
	// time because the WAL file and compaction channel are fixed infrastructure.
	streamerFactory func() *Streamer
	// replServer is the gRPC server implementation whose active flag is toggled.
	replServer *ManagedReplicationServer
	// stopStreamer cancels the current leader streaming loop. nil when inactive.
	stopStreamer context.CancelFunc
}

// NewReplicationManager creates a ReplicationManager.
// streamerFactory is called each time this node becomes leader.
func NewReplicationManager(
	streamerFactory func() *Streamer,
	replServer *ManagedReplicationServer,
) *ReplicationManager {
	return &ReplicationManager{
		streamerFactory: streamerFactory,
		replServer:      replServer,
	}
}

// BecomeLeader activates WAL streaming. Safe to call from any goroutine.
// Idempotent: calling while already leader is a no-op.
func (m *ReplicationManager) BecomeLeader(ctx context.Context) error {
	// Cancel any existing streamer first (safety: previous leader loop).
	m.stopCurrent()

	streamer := m.streamerFactory()
	m.replServer.Activate(streamer)

	slog.Info("replication: became leader — StreamWAL now active")
	return nil
}

// BecomeFollower deactivates WAL streaming and stops any active streamer.
// Safe to call from any goroutine. Idempotent.
func (m *ReplicationManager) BecomeFollower(ctx context.Context) error {
	m.stopCurrent()
	m.replServer.Deactivate()

	slog.Info("replication: became follower — StreamWAL deactivated")
	return nil
}

// BecomeStandalone is a no-op replication mode for single-node deployments.
func (m *ReplicationManager) BecomeStandalone() error {
	m.stopCurrent()
	m.replServer.Deactivate()

	slog.Info("replication: standalone mode — replication disabled")
	return nil
}

// stopCurrent cancels the active streamer context if one exists.
func (m *ReplicationManager) stopCurrent() {
	if m.stopStreamer != nil {
		m.stopStreamer()
		m.stopStreamer = nil
	}
}

// ManagedReplicationServer wraps ReplicationServer with an atomic active flag.
// When inactive, StreamWAL returns codes.Unavailable (null-object pattern).
// This allows the server to always be registered with grpc.Server while
// toggling live behaviour without re-registration.
type ManagedReplicationServer struct {
	pb.UnimplementedReplicationServiceServer

	// active == 1 when this node is acting as a leader and should stream WAL.
	active atomic.Int32

	// streamer is swapped atomically (pointer) on each BecomeLeader call.
	// Reads/writes are protected by the atomic active flag ensuring visibility.
	streamer atomic.Pointer[Streamer]
}

// NewManagedReplicationServer creates an inactive ManagedReplicationServer.
func NewManagedReplicationServer() *ManagedReplicationServer {
	return &ManagedReplicationServer{}
}

// Activate enables request processing with the given Streamer.
func (s *ManagedReplicationServer) Activate(streamer *Streamer) {
	s.streamer.Store(streamer)
	s.active.Store(1)
}

// Deactivate disables request processing. In-flight StreamWAL calls will drain
// via their existing context cancellation.
func (s *ManagedReplicationServer) Deactivate() {
	s.active.Store(0)
	s.streamer.Store(nil)
}

// StreamWAL implements pb.ReplicationServiceServer.
// Returns codes.Unavailable when this node is not the current leader.
func (s *ManagedReplicationServer) StreamWAL(req *pb.StreamWALRequest, stream pb.ReplicationService_StreamWALServer) error {
	if s.active.Load() == 0 {
		return status.Error(codes.Unavailable, "replication: this node is not the current leader")
	}

	streamer := s.streamer.Load()
	if streamer == nil {
		return status.Error(codes.Unavailable, "replication: streamer not initialised")
	}

	slog.Info("replication: replica connected",
		"replica_id", req.ReplicaId,
		"start_offset", req.StartOffset,
	)

	err := streamer.Stream(stream.Context(), req.StartOffset, func(entry RawEntry) error {
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
