package raft

// E2E test for Raft leader failover (ADR-015 §4, ADR-010 §5.2).
//
// Invariants verified:
//   - INV-F1: After the current leader is killed, the remaining two nodes elect
//     a new single leader within the election timeout window.
//   - INV-F2: Entries committed before the leader failure are durable on the
//     surviving nodes — no data loss for already-committed writes.
//   - INV-F3: The cluster accepts new writes via the new leader after failover.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// TestLeaderFailoverE2E is the primary leader-failover scenario:
//
//  1. Start a 3-node cluster and wait for leader election.
//  2. Write a key-value pair through the leader and wait for full replication.
//  3. Kill the leader (cancel context + stop gRPC server).
//  4. Wait for one of the remaining two nodes to win re-election (INV-F1).
//  5. Verify the pre-failover key is present on the new leader (INV-F2).
//  6. Propose a new entry through the new leader and confirm it replicates to
//     all surviving nodes (INV-F3).
func TestLeaderFailoverE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping E2E failover test in short mode")
	}

	nodes := startCluster(t, 3)

	// --- Phase 1: elect leader, write key ---

	leader := waitForLeader(t, nodes, 5*time.Second)
	t.Logf("initial leader: %s (addr %s)", leader.id, leader.addr)

	cmd := RaftKVCommand{Op: "set", Key: "failover-key", Value: "failover-value"}
	data, err := json.Marshal(cmd)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	preFailoverIdx, _, isLeader := leader.node.Propose(data)
	if !isLeader {
		t.Fatal("Propose returned isLeader=false on the elected leader")
	}

	// All nodes must apply the pre-failover entry (guarantees durability on 3/3).
	for _, n := range nodes {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := n.sm.WaitForIndex(ctx, preFailoverIdx); err != nil {
			cancel()
			t.Fatalf("node %s WaitForIndex(%d): %v", n.id, preFailoverIdx, err)
		}
		cancel()
	}
	t.Logf("all nodes applied pre-failover entry at index %d", preFailoverIdx)

	// --- Phase 2: kill the leader ---

	t.Logf("killing leader %s", leader.id)
	leader.cancel()
	leader.grpcSrv.Stop()
	// t.Cleanup already registered by startCluster — double-stop is safe.

	// --- Phase 3: re-election among surviving nodes ---

	remaining := make([]*testClusterNode, 0, 2)
	for _, n := range nodes {
		if n.id != leader.id {
			remaining = append(remaining, n)
		}
	}

	// With 2 of 3 nodes alive, a quorum (2) can elect a new leader.
	// Allow up to 3 election timeouts (generous, ~3–9 s).
	newLeader := waitForLeader(t, remaining, 10*time.Second)
	t.Logf("new leader elected: %s — INV-F1 PASS", newLeader.id)

	// --- Phase 4: INV-F2 — pre-failover key must survive on new leader ---

	got, ok := newLeader.sm.Get("failover-key")
	if !ok || got != "failover-value" {
		t.Errorf("INV-F2 FAIL: new leader %s failover-key=%q ok=%v, want %q",
			newLeader.id, got, ok, "failover-value")
	} else {
		t.Logf("INV-F2 PASS: new leader %s has failover-key=%q", newLeader.id, got)
	}

	// Survivor (non-leader) must also have it.
	for _, n := range remaining {
		if n.id == newLeader.id {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = n.sm.WaitForIndex(ctx, preFailoverIdx)
		cancel()

		got, ok := n.sm.Get("failover-key")
		if !ok || got != "failover-value" {
			t.Errorf("INV-F2 FAIL: survivor %s failover-key=%q ok=%v, want %q",
				n.id, got, ok, "failover-value")
		} else {
			t.Logf("INV-F2 PASS: survivor %s has failover-key=%q", n.id, got)
		}
	}

	// --- Phase 5: INV-F3 — cluster accepts new writes after failover ---

	for i := 0; i < 3; i++ {
		key := fmt.Sprintf("post-failover-key-%d", i)
		val := fmt.Sprintf("post-failover-val-%d", i)

		cmd2 := RaftKVCommand{Op: "set", Key: key, Value: val}
		data2, _ := json.Marshal(cmd2)
		idx2, _, isLeader2 := newLeader.node.Propose(data2)
		if !isLeader2 {
			t.Fatalf("INV-F3 FAIL: new leader %s lost leadership during post-failover write", newLeader.id)
		}

		for _, n := range remaining {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := n.sm.WaitForIndex(ctx, idx2); err != nil {
				cancel()
				t.Fatalf("INV-F3 FAIL: survivor %s WaitForIndex(%d): %v", n.id, idx2, err)
			}
			cancel()

			got, ok := n.sm.Get(key)
			if !ok || got != val {
				t.Errorf("INV-F3 FAIL: survivor %s %s=%q ok=%v, want %q",
					n.id, key, got, ok, val)
			}
		}
	}
	t.Logf("INV-F3 PASS: cluster accepted 3 new writes after failover")
}
