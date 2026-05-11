package raft_test

import (
	"testing"

	infraraft "github.com/junyoung/core-x/internal/infrastructure/raft"
)

// stubNode returns a non-nil *RaftNode suitable for registry tests.
// It does not start the node; only the pointer identity matters here.
func stubNode(t *testing.T) *infraraft.RaftNode {
	t.Helper()
	// NewRaftNode requires at least a nodeID. Passing nil for optional
	// dependencies is acceptable because we never call Run on this node.
	return infraraft.NewRaftNode("stub-node", nil, nil, nil)
}

func TestRaftGroupRegistry_RegisterAndGet(t *testing.T) {
	reg := infraraft.NewRaftGroupRegistry()
	node := stubNode(t)

	reg.Register("partition-0", node)

	got, ok := reg.Get("partition-0")
	if !ok {
		t.Fatal("Get: expected ok=true for registered partition")
	}
	if got != node {
		t.Errorf("Get: returned unexpected node pointer: got %p, want %p", got, node)
	}
}

func TestRaftGroupRegistry_GetMissing(t *testing.T) {
	reg := infraraft.NewRaftGroupRegistry()

	got, ok := reg.Get("nonexistent")
	if ok {
		t.Fatal("Get: expected ok=false for unregistered partition")
	}
	if got != nil {
		t.Errorf("Get: expected nil node for unregistered partition, got %p", got)
	}
}

func TestRaftGroupRegistry_All_ReturnsCopy(t *testing.T) {
	reg := infraraft.NewRaftGroupRegistry()
	n0 := stubNode(t)
	n1 := stubNode(t)

	reg.Register("p0", n0)
	reg.Register("p1", n1)

	snapshot := reg.All()
	if len(snapshot) != 2 {
		t.Fatalf("All: expected 2 entries, got %d", len(snapshot))
	}
	if snapshot["p0"] != n0 {
		t.Errorf("All: p0 mismatch")
	}
	if snapshot["p1"] != n1 {
		t.Errorf("All: p1 mismatch")
	}

	// Mutating the returned map must not affect the registry.
	delete(snapshot, "p0")
	snapshot["injected"] = stubNode(t)

	all2 := reg.All()
	if len(all2) != 2 {
		t.Errorf("All: registry corrupted by external map mutation; got %d entries, want 2", len(all2))
	}
	if _, ok := all2["p0"]; !ok {
		t.Error("All: p0 was unexpectedly removed from registry")
	}
	if _, ok := all2["injected"]; ok {
		t.Error("All: external key 'injected' leaked into registry")
	}
}

func TestRaftGroupRegistry_RegisterIdempotent(t *testing.T) {
	reg := infraraft.NewRaftGroupRegistry()
	first := stubNode(t)
	second := stubNode(t)

	reg.Register("p0", first)
	// Second Register on the same ID must be a no-op (INV-RGR1).
	reg.Register("p0", second)

	got, ok := reg.Get("p0")
	if !ok {
		t.Fatal("Get after duplicate Register: expected ok=true")
	}
	if got != first {
		t.Errorf("Register: second call must not overwrite first; got %p, want %p", got, first)
	}
}

func TestRaftGroupRegistry_RegisterNilPanics(t *testing.T) {
	reg := infraraft.NewRaftGroupRegistry()

	defer func() {
		if r := recover(); r == nil {
			t.Error("Register with nil node: expected panic, got none")
		}
	}()
	reg.Register("p0", nil)
}
