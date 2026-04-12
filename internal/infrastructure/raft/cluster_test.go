package raft

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"testing"
	"time"

	pb "github.com/junyoung/core-x/proto/pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// testClusterNode holds all components for a single node in a test cluster.
type testClusterNode struct {
	id      string
	node    *RaftNode
	sm      *KVStateMachine
	grpcSrv *grpc.Server
	addr    string
	cancel  context.CancelFunc
}

// startCluster spins up count RaftNodes connected via real loopback gRPC.
// Each node gets a KVStateMachine consuming its ApplyCh.
// t.Cleanup handles shutdown.
func startCluster(t *testing.T, count int) []*testClusterNode {
	t.Helper()

	// Step 1: bind listeners to get addresses before dialing.
	listeners := make([]net.Listener, count)
	addrs := make([]string, count)
	for i := 0; i < count; i++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		listeners[i] = ln
		addrs[i] = ln.Addr().String()
	}

	// Step 2: create nodes with peer clients pointing to other nodes.
	nodes := make([]*testClusterNode, count)
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("n%d", i+1)

		peerAddrs := make([]string, 0, count-1)
		for j := 0; j < count; j++ {
			if j != i {
				peerAddrs = append(peerAddrs, addrs[j])
			}
		}
		peers, err := NewPeerClients(peerAddrs)
		if err != nil {
			t.Fatalf("NewPeerClients: %v", err)
		}

		raftNode := NewRaftNode(id, peers, nil, nil)
		grpcSrv := grpc.NewServer()
		pb.RegisterRaftServiceServer(grpcSrv, NewRaftServer(raftNode))
		go grpcSrv.Serve(listeners[i]) //nolint:errcheck

		ctx, cancel := context.WithCancel(context.Background())
		sm := NewKVStateMachine()
		go raftNode.Run(ctx)
		go sm.Run(ctx, raftNode.ApplyCh())

		nodes[i] = &testClusterNode{
			id:      id,
			node:    raftNode,
			sm:      sm,
			grpcSrv: grpcSrv,
			addr:    addrs[i],
			cancel:  cancel,
		}
	}

	t.Cleanup(func() {
		for _, n := range nodes {
			n.cancel()
			n.grpcSrv.Stop()
		}
	})

	return nodes
}

// waitForLeader polls until exactly one node reports RoleLeader.
// Returns the leader node or fails the test after timeout.
func waitForLeader(t *testing.T, nodes []*testClusterNode, timeout time.Duration) *testClusterNode {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var leaders []*testClusterNode
		for _, n := range nodes {
			if n.node.Role() == RoleLeader {
				leaders = append(leaders, n)
			}
		}
		if len(leaders) == 1 {
			return leaders[0]
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no single leader elected within timeout")
	return nil
}

// TestCluster_ElectsLeader verifies that a 3-node cluster elects exactly one leader.
func TestCluster_ElectsLeader(t *testing.T) {
	nodes := startCluster(t, 3)
	leader := waitForLeader(t, nodes, 5*time.Second)
	t.Logf("leader elected: %s", leader.id)
}

// TestCluster_ProposeAndReplicate proposes a KV command via the leader and
// verifies all three state machines apply it.
func TestCluster_ProposeAndReplicate(t *testing.T) {
	nodes := startCluster(t, 3)
	leader := waitForLeader(t, nodes, 5*time.Second)

	cmd := RaftKVCommand{Op: "set", Key: "cluster-key", Value: "hello-raft"}
	data, _ := json.Marshal(cmd)
	index, _, isLeader := leader.node.Propose(data)
	if !isLeader {
		t.Fatal("Propose returned isLeader=false on the elected leader")
	}

	// All state machines should eventually apply the entry.
	for _, n := range nodes {
		waitCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := n.sm.WaitForIndex(waitCtx, index); err != nil {
			cancel()
			t.Fatalf("node %s WaitForIndex(%d): %v", n.id, index, err)
		}
		cancel()

		v, ok := n.sm.Get("cluster-key")
		if !ok || v != "hello-raft" {
			t.Fatalf("node %s: expected cluster-key=hello-raft, got %q ok=%v", n.id, v, ok)
		}
	}
}

// TestCluster_LeaderIDKnownToFollowers verifies that followers learn the
// leader's ID after receiving AppendEntries heartbeats.
func TestCluster_LeaderIDKnownToFollowers(t *testing.T) {
	nodes := startCluster(t, 3)
	leader := waitForLeader(t, nodes, 5*time.Second)

	// Give heartbeats time to propagate.
	time.Sleep(2 * HeartbeatInterval)

	for _, n := range nodes {
		if n.id == leader.id {
			continue // leader knows itself
		}
		gotID := n.node.LeaderID()
		if gotID != leader.id {
			t.Errorf("follower %s: LeaderID()=%q, want %q", n.id, gotID, leader.id)
		}
	}
}

// dialInsecure is a helper used by integration tests to verify gRPC connectivity.
func dialInsecure(addr string) (*grpc.ClientConn, error) {
	return grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
}
