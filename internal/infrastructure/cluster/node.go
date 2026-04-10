// Package cluster implements consistent hashing ring and node membership
// for Phase 3 distributed partitioning.
package cluster

import "sync/atomic"

// Node represents a single peer in the cluster.
// healthy is updated by the membership health prober and read by the HTTP
// forwarding layer — both happen concurrently, so atomic.Bool is used.
type Node struct {
	ID   string // unique identifier, e.g. "node-1"
	Addr string // gRPC listen address, e.g. "localhost:9001"

	healthy atomic.Bool
}

// NewNode creates a Node and marks it healthy by default.
// The caller is responsible for registering the node with membership probing.
func NewNode(id, addr string) *Node {
	n := &Node{ID: id, Addr: addr}
	n.healthy.Store(true)
	return n
}

// IsHealthy reports whether the node is currently considered reachable.
func (n *Node) IsHealthy() bool {
	return n.healthy.Load()
}

// setHealthy updates the node's health state. Called only by membership prober.
func (n *Node) setHealthy(v bool) {
	n.healthy.Store(v)
}
