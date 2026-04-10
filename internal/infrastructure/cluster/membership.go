package cluster

import (
	"context"
	"log/slog"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	defaultProbeInterval   = 5 * time.Second
	defaultProbeTimeout    = 2 * time.Second
	unhealthyAfterFailures = 2
)

// Membership manages node health probing and ring updates.
// A background goroutine periodically checks each node's gRPC connectivity
// and updates the node's atomic healthy flag.
type Membership struct {
	ring          *Ring
	probeInterval time.Duration
	probeTimeout  time.Duration
}

// NewMembership creates a Membership for the given ring.
func NewMembership(ring *Ring) *Membership {
	return &Membership{
		ring:          ring,
		probeInterval: defaultProbeInterval,
		probeTimeout:  defaultProbeTimeout,
	}
}

// Start launches the health probe goroutine. It stops when ctx is cancelled.
func (m *Membership) Start(ctx context.Context) {
	go m.probeLoop(ctx)
}

func (m *Membership) probeLoop(ctx context.Context) {
	ticker := time.NewTicker(m.probeInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.probeAll()
		}
	}
}

func (m *Membership) probeAll() {
	for _, node := range m.ring.Nodes() {
		healthy := m.probe(node)
		wasHealthy := node.IsHealthy()
		node.setHealthy(healthy)

		if wasHealthy != healthy {
			if healthy {
				slog.Info("node recovered", "node_id", node.ID, "addr", node.Addr)
			} else {
				slog.Warn("node marked unhealthy", "node_id", node.ID, "addr", node.Addr)
			}
		}
	}
}

// probe attempts a gRPC connection to node and returns true if reachable.
// It uses a short timeout to avoid blocking the probe loop.
func (m *Membership) probe(node *Node) bool {
	ctx, cancel := context.WithTimeout(context.Background(), m.probeTimeout)
	defer cancel()

	conn, err := grpc.NewClient(
		node.Addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return false
	}
	defer conn.Close()

	// Trigger connection attempt.
	conn.Connect()

	// Poll connectivity state until Ready or timeout.
	for {
		state := conn.GetState()
		if state == connectivity.Ready {
			return true
		}
		if !conn.WaitForStateChange(ctx, state) {
			// ctx expired or cancelled.
			return false
		}
	}
}
