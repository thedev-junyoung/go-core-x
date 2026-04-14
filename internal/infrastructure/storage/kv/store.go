// Store implements a KV store backed by WAL + in-memory hash index (Bitcask model).
//
// Interface implementation:
//   - application/ingestion.WALWriter: WriteEvent(e *domain.Event) error
//
// Design:
//   - WriteEvent: WAL write (via writer.WriteEventOffset) + index update (atomic pair)
//   - Get: index lookup → ReadRecordAt(offset) → event decode
//   - Recover: WAL replay → index rebuild + recovery callback
//   - Close: cleanup (readFile)
//
// Crash recovery guarantee:
//   - Recover() rebuilds index by replaying WAL
//   - WAL offset is always correct because writer.WriteEventOffset returns pre-write offset
//   - If crash occurs after WriteEventOffset returns, data is in WAL → Recover sees it
//   - If crash occurs before WriteEventOffset returns, Write never happened → no index entry needed
package kv

import (
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/junyoung/core-x/internal/domain"
	"github.com/junyoung/core-x/internal/infrastructure/storage/wal"
)

// Store implements a KV store using WAL (durable) + HashIndex (fast lookups).
//
// Fields:
//   - index: in-memory hash index (source → WAL offset)
//   - writer: WAL writer (concrete type; same infrastructure layer)
//   - readFile: read-only file handle for Get (ReadAt access)
//   - walPath: path to WAL file (used for reopening on recovery if needed)
//
// Thread-safety:
//   - WriteEvent: Uses writer.mu internally (via WriteEventOffset)
//   - Get: Uses mu (RLock) to snapshot index/readFile pointers; index.mu for key lookup
//   - Compact: Uses writer.RunExclusiveSwap (blocks writes) + mu (Lock) to swap pointers
//   - Recover: Single-threaded (startup path only)
type Store struct {
	mu       sync.RWMutex // protects index and readFile pointer swaps during Compact
	index    *HashIndex
	writer   *wal.Writer
	readFile *os.File
	walPath  string

	// raftLastApplied is the last Raft log index applied to this KV store.
	// Updated atomically by WriteKV/DeleteKV (raft_store.go).
	// Zero on fresh start; restored from WAL by RecoverKV.
	raftLastApplied int64
}

// NewStore creates a KV store with WAL backing and in-memory index.
//
// Parameters:
//   - writer: initialized WAL writer (must not be nil)
//   - walPath: path to WAL file (used for read-only access in Get)
//   - maxKeys: maximum number of keys in index; prevents unbounded growth
//
// Returns error if readFile cannot be opened (WAL file may not exist yet on first run).
func NewStore(writer *wal.Writer, walPath string, maxKeys int) (*Store, error) {
	// Open read-only file handle for Get operations (ReadAt)
	// File must exist (writer has already created it in O_APPEND|O_CREATE mode)
	readFile, err := os.Open(walPath)
	if err != nil {
		return nil, fmt.Errorf("kv: failed to open wal for reading: %w", err)
	}

	return &Store{
		index:    NewHashIndex(maxKeys),
		writer:   writer,
		readFile: readFile,
		walPath:  walPath,
	}, nil
}

// WriteEvent implements application/ingestion.WALWriter interface.
//
// Procedure:
//   1. Call writer.WriteEventOffset → returns (offset, error)
//   2. On success: index.Set(e.Source, offset)
//   3. On failure: return error immediately (no index update)
//
// Atomicity: WriteEventOffset is already atomic (mu-protected).
// Index.Set acquires its own lock, creating a brief gap where WAL has data but index doesn't.
// This is acceptable because:
//   - Gap is measured in nanoseconds
//   - Crash during gap → Recover replays WAL → index rebuilt correctly
//   - In normal operation, gap closes immediately
//
// Semantics (Bitcask):
//   - Same source, multiple writes: offset is updated (latest-write-wins)
//   - Different sources: independent entries
//
// Returns error if:
//   - writer.WriteEventOffset fails (disk error, sync error)
//   - index.Set fails (ErrIndexFull: maxKeys exceeded)
func (s *Store) WriteEvent(e *domain.Event) error {
	// Step 1: Write to WAL, get offset
	offset, err := s.writer.WriteEventOffset(e)
	if err != nil {
		return err
	}

	// Step 2: Update index
	if err := s.index.Set(e.Source, offset); err != nil {
		// Index is full. WAL has data but not indexed.
		// Recover will still find it by replaying WAL.
		// But subsequent Get(source) will fail until a different source evicts it.
		// This is an overload condition; caller (IngestionService) may return 429.
		return err
	}

	return nil
}

// Get retrieves an event by source key.
//
// Procedure:
//   1. index.Get(key) → offset (RLock, 0 alloc)
//   2. ReadRecordAt(readFile, offset) → WAL record
//   3. wal.DecodeEvent(record.Data) → *domain.Event
//   4. Set ReceivedAt from record.Timestamp
//   5. Return event
//
// Returns error if:
//   - key not found: ErrNotFound
//   - ReadRecordAt fails (corruption, truncation)
//   - DecodeEvent fails (malformed payload)
//
// Allocation:
//   - index.Get: 0 alloc (RLock + map lookup)
//   - ReadRecordAt: allocates payload buffer (~payload size)
//   - DecodeEvent: allocates Event struct + strings (unavoidable for value return)
//   - Net: O(payload_size) allocations (necessary for value semantics)
//
// Concurrency: Safe for concurrent calls. mu.RLock snapshots index and readFile
// pointers so Compact() can swap them without a race.
func (s *Store) Get(key string) (*domain.Event, error) {
	// Snapshot index and readFile pointers under read lock.
	// Compact() may replace these pointers; snapshot ensures we use a consistent pair.
	s.mu.RLock()
	idx := s.index
	rf := s.readFile
	s.mu.RUnlock()

	// Step 1: Lookup index
	offset, found := idx.Get(key)
	if !found {
		return nil, ErrKeyNotFound
	}

	// Step 2: Read record from WAL at offset
	record, err := wal.ReadRecordAt(rf, offset)
	if err != nil {
		return nil, fmt.Errorf("kv: failed to read record at offset %d: %w", offset, err)
	}

	// Step 3: Decode event from record
	event, err := wal.DecodeEvent(record.Data)
	if err != nil {
		return nil, fmt.Errorf("kv: failed to decode event at offset %d: %w", offset, err)
	}

	// Step 4: Restore timestamp
	event.ReceivedAt = record.Timestamp

	return event, nil
}

// Recover rebuilds the index by replaying the WAL file.
//
// Called on server startup to reconstruct the in-memory index from durable WAL.
// Uses a callback pattern to allow different recovery strategies:
//   - kv.Store: index rebuild only
//   - cmd/main.go: index rebuild + workerPool re-submission (processing recovery)
//
// Procedure:
//   1. Open WAL reader
//   2. Iterate over all complete records:
//      a. Decode event
//      b. index.Set(source, offset)
//      c. Invoke onRecover callback (if not nil)
//   3. Handle truncation (normal in crash) vs corruption (fatal)
//   4. Return count of recovered events
//
// Offset calculation:
//   - offset starts at 0
//   - For each record: offset is current position before parsing
//   - After parsing: offset += RecordMinSize + len(record.Data)
//   - This avoids bufio buffering (sequential scan instead of buffered)
//
// Error handling:
//   - ErrTruncated: normal (last incomplete record) → treated as success
//   - ErrCorrupted / ErrChecksumMismatch: fatal → returned as error
//   - WAL file not found: treated as "nothing to recover" (return 0, nil)
//
// Parameters:
//   - onRecover: optional callback invoked for each recovered event (for downstream processing)
//     Passing nil is valid (skips callback). Used in tests.
//
// Returns: (count of recovered events, error)
func (s *Store) Recover(onRecover func(*domain.Event)) (int, error) {
	// Check if WAL file exists
	if _, err := os.Stat(s.walPath); os.IsNotExist(err) {
		return 0, nil // No file to recover; normal on first run
	}

	// Open WAL reader
	reader, err := wal.NewReader(s.walPath)
	if err != nil {
		return 0, fmt.Errorf("kv: failed to open wal reader: %w", err)
	}
	defer reader.Close()

	count := 0
	offset := int64(0)

	// Replay WAL
	for reader.Scan() {
		record := reader.Record()

		// Decode event
		event, err := wal.DecodeEvent(record.Data)
		if err != nil {
			return count, fmt.Errorf("kv: recovery decode failed at offset %d: %w", offset, err)
		}

		// Restore timestamp
		event.ReceivedAt = record.Timestamp

		// Update index
		if err := s.index.Set(event.Source, offset); err != nil {
			// Index full during recovery; log but continue
			// Data is still in WAL; future recovery may evict other keys
			return count, fmt.Errorf("kv: recovery index full at offset %d: %w", offset, err)
		}

		// Invoke callback
		if onRecover != nil {
			onRecover(event)
		}

		// Calculate offset for next record
		// RecordFormat: [Header:16][Payload:N][Checksum:4]
		recordSize := int64(wal.RecordHeaderSize) + int64(len(record.Data)) + int64(wal.RecordChecksumSize)
		offset += recordSize

		count++
	}

	// Handle read errors
	if err := reader.Err(); err != nil {
		// ErrTruncated: normal (last incomplete record during crash)
		if err == wal.ErrTruncated {
			return count, nil
		}
		// ErrCorrupted, ErrChecksumMismatch: fatal
		return count, fmt.Errorf("kv: recovery read failed: %w", err)
	}

	return count, nil
}

// Len returns the number of entries in the index.
// Used for monitoring (e.g., logging recovery results).
func (s *Store) Len() int {
	return s.index.Len()
}

// Close closes the read-only file handle.
// Call before shutting down (see cmd/main.go Shutdown sequence).
func (s *Store) Close() error {
	if s.readFile != nil {
		return s.readFile.Close()
	}
	return nil
}

// Error definitions
var (
	ErrKeyNotFound = errors.New("kv: key not found")
)
