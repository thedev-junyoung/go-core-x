package raft

import (
	"log/slog"
	"sync"
)

// PartitionID uniquely identifies a Raft partition (shard) within the cluster.
// It is a string to support both numeric ("0", "1") and named ("shard-us-east") identifiers.
type PartitionID string

// RaftGroupRegistry is a thread-safe registry that maps PartitionID → *RaftNode.
//
// Invariants:
//   - INV-RGR1 (Immutable entries): Once a PartitionID is registered, the mapping
//     is never updated or removed during the lifetime of the process. This prevents
//     a torn-read hazard where a caller holds a stale *RaftNode pointer.
//   - INV-RGR2 (Read copy): All() returns a shallow copy of the internal map so
//     callers cannot mutate the registry's state by writing to the returned map.
//   - INV-RGR3 (No nil nodes): Register panics on a nil *RaftNode to prevent
//     callers from storing unusable entries.
type RaftGroupRegistry struct {
	mu    sync.RWMutex
	nodes map[PartitionID]*RaftNode
}

// NewRaftGroupRegistry creates an empty RaftGroupRegistry.
func NewRaftGroupRegistry() *RaftGroupRegistry {
	return &RaftGroupRegistry{
		nodes: make(map[PartitionID]*RaftNode),
	}
}

// Register associates id with node.
//
// Panics if node is nil (INV-RGR3).
// If id is already registered the call is a no-op (INV-RGR1); callers that need
// to detect duplicate registration should call Get before Register.
func (r *RaftGroupRegistry) Register(id PartitionID, node *RaftNode) {
	if node == nil {
		panic("raft: RaftGroupRegistry.Register called with nil RaftNode")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.nodes[id]; exists {
		// INV-RGR1: immutable entries — duplicate is silently ignored,
		// but we warn so operators can detect misconfiguration at startup.
		slog.Warn("raft: RaftGroupRegistry.Register: duplicate partition ID ignored",
			"partition", id)
		return
	}
	r.nodes[id] = node
}

// Get returns the *RaftNode registered for id.
// Returns (nil, false) if id has not been registered.
func (r *RaftGroupRegistry) Get(id PartitionID) (*RaftNode, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	node, ok := r.nodes[id]
	return node, ok
}

// All returns a shallow copy of the internal map (INV-RGR2).
// The caller may iterate or read the returned map freely; writes to it do not
// affect the registry.
func (r *RaftGroupRegistry) All() map[PartitionID]*RaftNode {
	r.mu.RLock()
	defer r.mu.RUnlock()
	// zero-alloc: copy is proportional to the number of registered partitions
	// which is bounded by cluster configuration, not request rate.
	out := make(map[PartitionID]*RaftNode, len(r.nodes))
	for id, node := range r.nodes {
		out[id] = node
	}
	return out
}
