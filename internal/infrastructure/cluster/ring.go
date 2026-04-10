package cluster

import (
	"hash/fnv"
	"sort"
	"strconv"
	"sync"
)

const defaultVnodeCount = 150

// Ring is a thread-safe consistent hash ring using virtual nodes.
//
// Each physical node is mapped to vnodeCount points on a uint32 ring.
// Lookup uses binary search — O(log(N*vnodeCount)) per call.
// With 150 vnodes and 10 nodes: ~11 comparisons ≈ 10 ns.
//
// FNV-32a is used for hashing: better distribution than CRC32 for short
// string keys, and cheaper than SHA-256.
type Ring struct {
	mu         sync.RWMutex
	vnodeCount int
	points     []uint32       // sorted vnode hash values
	nodeByHash map[uint32]*Node // vnode hash → physical node
	nodes      map[string]*Node // nodeID → physical node
}

// NewRing creates an empty Ring with the given vnodeCount per physical node.
// If vnodeCount <= 0, defaultVnodeCount (150) is used.
func NewRing(vnodeCount int) *Ring {
	if vnodeCount <= 0 {
		vnodeCount = defaultVnodeCount
	}
	return &Ring{
		vnodeCount: vnodeCount,
		nodeByHash: make(map[uint32]*Node),
		nodes:      make(map[string]*Node),
	}
}

// AddNode registers a node and inserts its vnodes into the ring.
// If a node with the same ID already exists, it is replaced.
func (r *Ring) AddNode(n *Node) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.nodes[n.ID]; exists {
		r.removeNodeLocked(n.ID)
	}

	r.nodes[n.ID] = n
	for i := range r.vnodeCount {
		h := vnodeHash(n.ID, i)
		r.points = append(r.points, h)
		r.nodeByHash[h] = n
	}
	sort.Slice(r.points, func(i, j int) bool { return r.points[i] < r.points[j] })
}

// RemoveNode removes a node and its vnodes from the ring.
func (r *Ring) RemoveNode(nodeID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.removeNodeLocked(nodeID)
}

// removeNodeLocked removes node vnodes. Must be called with r.mu held.
func (r *Ring) removeNodeLocked(nodeID string) {
	if _, exists := r.nodes[nodeID]; !exists {
		return
	}
	delete(r.nodes, nodeID)

	for i := range r.vnodeCount {
		h := vnodeHash(nodeID, i)
		delete(r.nodeByHash, h)
	}

	// Rebuild points slice without the removed node's hashes.
	newPoints := r.points[:0]
	for _, p := range r.points {
		if _, ok := r.nodeByHash[p]; ok {
			newPoints = append(newPoints, p)
		}
	}
	r.points = newPoints
}

// Lookup returns the node responsible for the given key.
// Returns (nil, false) when the ring is empty.
// The returned node may be unhealthy — callers must check IsHealthy().
func (r *Ring) Lookup(key string) (*Node, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if len(r.points) == 0 {
		return nil, false
	}

	h := keyHash(key)
	idx := sort.Search(len(r.points), func(i int) bool {
		return r.points[i] >= h
	})
	// Wrap around: if h is greater than all points, use the first point.
	if idx == len(r.points) {
		idx = 0
	}
	return r.nodeByHash[r.points[idx]], true
}

// Nodes returns a snapshot of all registered nodes.
func (r *Ring) Nodes() []*Node {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]*Node, 0, len(r.nodes))
	for _, n := range r.nodes {
		out = append(out, n)
	}
	return out
}

// Len returns the number of physical nodes in the ring.
func (r *Ring) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.nodes)
}

// vnodeHash computes the ring position for the i-th vnode of a physical node.
func vnodeHash(nodeID string, i int) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(nodeID + "#" + strconv.Itoa(i)))
	return h.Sum32()
}

// keyHash computes the ring position for a source key.
func keyHash(key string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return h.Sum32()
}
