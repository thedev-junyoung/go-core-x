package cluster

import (
	"fmt"
	"testing"
)

func TestRing_LookupEmpty(t *testing.T) {
	r := NewRing(10)
	node, ok := r.Lookup("any-key")
	if ok || node != nil {
		t.Fatalf("expected (nil, false) on empty ring, got (%v, %v)", node, ok)
	}
}

func TestRing_SingleNode(t *testing.T) {
	r := NewRing(10)
	n := NewNode("node-1", "localhost:9001")
	r.AddNode(n)

	got, ok := r.Lookup("some-source")
	if !ok {
		t.Fatal("expected ok=true, got false")
	}
	if got.ID != "node-1" {
		t.Fatalf("expected node-1, got %s", got.ID)
	}
}

func TestRing_MultiNode_Deterministic(t *testing.T) {
	r := NewRing(150)
	r.AddNode(NewNode("node-1", "localhost:9001"))
	r.AddNode(NewNode("node-2", "localhost:9002"))
	r.AddNode(NewNode("node-3", "localhost:9003"))

	// Same key must always resolve to the same node.
	key := "user-abc"
	first, _ := r.Lookup(key)
	for i := 0; i < 100; i++ {
		got, ok := r.Lookup(key)
		if !ok || got.ID != first.ID {
			t.Fatalf("non-deterministic lookup: iter %d got %v, want %s", i, got, first.ID)
		}
	}
}

func TestRing_Distribution(t *testing.T) {
	r := NewRing(150)
	nodes := []string{"node-1", "node-2", "node-3"}
	for _, id := range nodes {
		r.AddNode(NewNode(id, "localhost:900"+id[len(id)-1:]))
	}

	counts := make(map[string]int, len(nodes))
	total := 100_000
	for i := range total {
		key := fmt.Sprintf("source-%d", i)
		n, ok := r.Lookup(key)
		if !ok {
			t.Fatal("unexpected empty ring")
		}
		counts[n.ID]++
	}

	// Each node should receive between 20% and 47% of keys (150 vnodes, 3 nodes).
	for _, id := range nodes {
		pct := float64(counts[id]) / float64(total) * 100
		if pct < 20 || pct > 47 {
			t.Errorf("node %s: %.1f%% of keys (expected 20%%–47%%)", id, pct)
		}
	}
}

func TestRing_RemoveNode(t *testing.T) {
	r := NewRing(150)
	r.AddNode(NewNode("node-1", "localhost:9001"))
	r.AddNode(NewNode("node-2", "localhost:9002"))

	r.RemoveNode("node-2")

	if r.Len() != 1 {
		t.Fatalf("expected 1 node after removal, got %d", r.Len())
	}
	for i := range 1000 {
		key := fmt.Sprintf("k-%d", i)
		n, ok := r.Lookup(key)
		if !ok {
			t.Fatal("ring should not be empty")
		}
		if n.ID == "node-2" {
			t.Fatalf("removed node still being returned for key %s", key)
		}
	}
}

func TestRing_AddNodeReplacement(t *testing.T) {
	r := NewRing(10)
	r.AddNode(NewNode("node-1", "localhost:9001"))

	// Adding the same ID again should replace, not duplicate.
	r.AddNode(NewNode("node-1", "localhost:9999"))

	if r.Len() != 1 {
		t.Fatalf("expected 1 node, got %d", r.Len())
	}
	n, _ := r.Lookup("any")
	if n.Addr != "localhost:9999" {
		t.Fatalf("expected updated addr, got %s", n.Addr)
	}
}
