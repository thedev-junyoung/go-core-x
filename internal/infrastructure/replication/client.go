package replication

import (
	"context"
	"io"
	"log/slog"
	"time"

	"google.golang.org/grpc"

	pb "github.com/junyoung/core-x/proto/pb"
)

// ReplicationClient connects to a primary's ReplicationService and feeds
// received WAL entries into a Receiver. It reconnects automatically on
// disconnection using exponential backoff.
type ReplicationClient struct {
	primaryAddr string
	replicaID   string
	receiver    *Receiver
	lag         *ReplicationLag
	dialOpts    []grpc.DialOption
}

// NewReplicationClient creates a new ReplicationClient.
func NewReplicationClient(primaryAddr, replicaID string, receiver *Receiver, lag *ReplicationLag, dialOpts ...grpc.DialOption) *ReplicationClient {
	return &ReplicationClient{
		primaryAddr: primaryAddr,
		replicaID:   replicaID,
		receiver:    receiver,
		lag:         lag,
		dialOpts:    dialOpts,
	}
}

// Run starts the replication loop. Returns when ctx is cancelled.
func (c *ReplicationClient) Run(ctx context.Context) error {
	backoff := 100 * time.Millisecond

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		err := c.runOnce(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			slog.Warn("replication: client error, reconnecting",
				"primary", c.primaryAddr,
				"err", err,
				"backoff", backoff,
			)
		}

		if ctx.Err() == nil {
			c.lag.IncReconnect()
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}

		backoff *= 2
		if backoff > 10*time.Second {
			backoff = 10 * time.Second
		}
	}
}

// runOnce performs a single connection attempt and streams until error or ctx done.
func (c *ReplicationClient) runOnce(ctx context.Context) error {
	conn, err := grpc.NewClient(c.primaryAddr, c.dialOpts...)
	if err != nil {
		return err
	}
	defer conn.Close()

	client := pb.NewReplicationServiceClient(conn)

	startOffset, err := c.receiver.PersistedOffset()
	if err != nil {
		return err
	}
	c.lag.UpdateReplica(startOffset)

	stream, err := client.StreamWAL(ctx, &pb.StreamWALRequest{
		ReplicaId:   c.replicaID,
		StartOffset: startOffset,
	})
	if err != nil {
		return err
	}

	slog.Info("replication: connected to primary",
		"primary", c.primaryAddr,
		"start_offset", startOffset,
	)

	for {
		entry, err := stream.Recv()
		if err == io.EOF || ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return err
		}

		if appErr := c.receiver.Append(RawEntry{
			Offset:     entry.Offset,
			RawData:    entry.RawData,
			RecordSize: entry.RecordSize,
		}); appErr != nil {
			return appErr
		}

		c.lag.UpdateReplica(entry.Offset + entry.RecordSize)
	}
}
