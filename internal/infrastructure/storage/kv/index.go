// Package kv implements a hash index based KV store using WAL as backing storage.
//
// Design: Bitcask model.
//   - WAL (Sequential Write-Ahead Log): durable storage
//   - HashIndex (in-memory map): O(1) key lookup
//   - Crash recovery: WAL replay rebuilds index
//
// Thread-safety: All public methods use RWMutex for concurrent read/write access.
// Zero-allocation: Get() returns (offset, bool) without allocations on critical path.
package kv

import (
	"errors"
	"sync"
)

// ErrIndexFull is returned when Set() exceeds maxKeys limit.
var ErrIndexFull = errors.New("kv: index full, max keys exceeded")

// HashIndex maps event source keys to their WAL offsets for O(1) lookup.
//
// Design rationale:
//   - map[string]int64: source (key) → WAL record start offset (value)
//   - Key is Event.Source (identifies event source/stream)
//   - Value is int64 offset: when multiple events from same source arrive,
//     later writes overwrite earlier offsets (latest-write-wins Bitcask semantics)
//   - maxKeys bound prevents unbounded memory growth in single-node deployments
//
// Thread-safety: RWMutex protects all map access.
// Get() uses RLock for concurrent reads; Set/Delete use Lock for exclusive writes.
//
// Memory model: Zero allocation on Get (returns primitive types).
// Set may allocate when map grows (hash table resize), but per Go's map semantics,
// this is amortized O(1) per insertion.
type HashIndex struct {
	mu      sync.RWMutex
	entries map[string]int64
	maxKeys int
}

// NewHashIndex creates a new hash index with a maximum key limit.
// maxKeys is used to prevent unbounded memory growth in single-node scenarios.
// Phase 3+ (distributed): keyspace sharding makes this irrelevant.
func NewHashIndex(maxKeys int) *HashIndex {
	return &HashIndex{
		entries: make(map[string]int64),
		maxKeys: maxKeys,
	}
}

// Set associates key with offset. Returns ErrIndexFull if maxKeys exceeded.
//
// Semantics (Bitcask write):
//   - If key exists: offset is updated (overwrite)
//   - If key is new: entry is added if Len() < maxKeys
//   - Overwrites are always allowed (do not increment Len())
//   - Returns ErrIndexFull only when adding NEW keys would exceed maxKeys
//
// Call sequence in Store.WriteEvent → writer.WriteEventOffset → index.Set(source, offset).
// Offline recovery in Store.Recover → index.Set(source, offset) for each WAL record.
func (idx *HashIndex) Set(key string, offset int64) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	// Check if this is a new key (not an overwrite)
	if _, exists := idx.entries[key]; !exists && len(idx.entries) >= idx.maxKeys {
		return ErrIndexFull
	}

	idx.entries[key] = offset
	return nil
}

// Get returns the offset associated with key, and true if found.
//
// Thread-safe read: Uses RLock; multiple goroutines can call concurrently.
// Zero allocation: Returns primitive types (int64, bool).
//
// Call sequence: Store.Get → index.Get → then ReadRecordAt(offset).
func (idx *HashIndex) Get(key string) (int64, bool) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	offset, exists := idx.entries[key]
	return offset, exists
}

// Delete removes an entry from the index.
//
// Used by compaction/cleanup phases (Phase 3+). Currently not used in Phase 2.
// Provided for API completeness.
func (idx *HashIndex) Delete(key string) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	delete(idx.entries, key)
}

// Snapshot returns a copy of all index entries as map[string]int64.
//
// Used by Compact() to iterate over all live keys without holding the lock
// for the duration of I/O. The snapshot is a point-in-time copy; writes that
// arrive after Snapshot() returns are not reflected.
func (idx *HashIndex) Snapshot() map[string]int64 {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	snap := make(map[string]int64, len(idx.entries))
	for k, v := range idx.entries {
		snap[k] = v
	}
	return snap
}

// Len returns the number of entries in the index.
//
// Used for monitoring and maxKeys enforcement.
// Snapshot value: may change immediately after return.
func (idx *HashIndex) Len() int {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	return len(idx.entries)
}
