// Package replication implements Phase 3b WAL streaming replication.
package replication

import "sync/atomic"

// ReplicationLag tracks the byte offset difference between primary and replica.
type ReplicationLag struct {
	primaryOffset  atomic.Int64
	replicaOffset  atomic.Int64
	reconnectCount atomic.Int64
}

// NewReplicationLag creates a new ReplicationLag with all counters at zero.
func NewReplicationLag() *ReplicationLag {
	return &ReplicationLag{}
}

// UpdatePrimary sets the latest offset streamed by the primary.
func (l *ReplicationLag) UpdatePrimary(offset int64) {
	l.primaryOffset.Store(offset)
}

// UpdateReplica sets the latest offset persisted by the replica.
func (l *ReplicationLag) UpdateReplica(offset int64) {
	l.replicaOffset.Store(offset)
}

// Bytes returns the current replication lag in bytes.
// Returns 0 if replica reported a higher offset than primary (race condition).
func (l *ReplicationLag) Bytes() int64 {
	v := l.primaryOffset.Load() - l.replicaOffset.Load()
	if v < 0 {
		return 0
	}
	return v
}

// IncReconnect increments the reconnect counter.
func (l *ReplicationLag) IncReconnect() {
	l.reconnectCount.Add(1)
}

// ReconnectCount returns the total number of replica reconnections.
func (l *ReplicationLag) ReconnectCount() int64 {
	return l.reconnectCount.Load()
}
