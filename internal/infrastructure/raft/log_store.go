package raft

import (
	"encoding/binary"
	"fmt"
	"os"
	"sync"

	walstore "github.com/junyoung/core-x/internal/infrastructure/storage/wal"
)

// Record type bytes for the WAL payload.
const (
	logRecordTypeEntry    = byte(0x01) // a Raft log entry
	logRecordTypeTruncate = byte(0x02) // truncate-suffix marker
)

// LogStore durably persists Raft log entries to stable storage.
//
// Implementations must guarantee that Append and TruncateSuffix are
// durable (fsync'd) before returning, because Raft safety depends on
// log entries surviving crashes.
type LogStore interface {
	// Append durably writes entry to stable storage.
	// Must be called with entries in ascending Index order.
	Append(entry LogEntry) error

	// TruncateSuffix marks all entries with Index >= fromIndex as discarded.
	// Used during AppendEntries conflict resolution (§5.3).
	TruncateSuffix(fromIndex int64) error

	// LoadAll reads all persisted entries and replays truncation markers,
	// returning the final correct log in index order.
	// Called once at startup for log recovery.
	LoadAll() ([]LogEntry, error)

	// Close releases underlying resources.
	Close() error
}

// WALLogStore is a LogStore backed by the existing wal.Writer infrastructure.
//
// On-disk format: each record is a WAL entry (magic + timestamp + CRC32)
// whose payload begins with a type byte:
//
//	typeEntry    [0x01] [Index:8 LE] [Term:8 LE] [DataLen:4 LE] [Data:N]
//	typeTruncate [0x02] [FromIndex:8 LE]
//
// Truncation uses an append-only tombstone: a typeTruncate record is appended
// and replayed during LoadAll to discard conflicting entries without rewriting
// the file. This keeps the write path simple and atomic.
//
// SyncImmediate policy is required: each Append/TruncateSuffix must be durable
// before returning so the caller can safely respond to the Raft leader.
type WALLogStore struct {
	path string
	w    *walstore.Writer
}

// NewWALLogStore opens or creates a Raft log WAL at path.
// SyncImmediate is used unconditionally: Raft log persistence must be durable
// per write to satisfy §5 safety requirements.
func NewWALLogStore(path string) (*WALLogStore, error) {
	w, err := walstore.NewWriter(walstore.Config{
		Path:       path,
		SyncPolicy: walstore.SyncImmediate,
	})
	if err != nil {
		return nil, fmt.Errorf("raft log store: open %s: %w", path, err)
	}
	return &WALLogStore{path: path, w: w}, nil
}

// Append encodes entry and durably writes it as a WAL record.
func (s *WALLogStore) Append(entry LogEntry) error {
	payload := encodeLogEntry(entry)
	if err := s.w.Write(payload); err != nil {
		return fmt.Errorf("raft log store: append index=%d: %w", entry.Index, err)
	}
	return nil
}

// TruncateSuffix appends a truncation marker for entries with Index >= fromIndex.
// LoadAll will replay this marker and discard the affected entries.
func (s *WALLogStore) TruncateSuffix(fromIndex int64) error {
	payload := encodeTruncate(fromIndex)
	if err := s.w.Write(payload); err != nil {
		return fmt.Errorf("raft log store: truncate from=%d: %w", fromIndex, err)
	}
	return nil
}

// LoadAll opens the WAL file for reading and replays all records.
// Returns an empty slice without error if the file does not exist yet (first startup).
// ErrTruncated (partial tail record from a pre-crash write) is silently ignored:
// the in-progress record had not been acknowledged, so dropping it is safe.
func (s *WALLogStore) LoadAll() ([]LogEntry, error) {
	r, err := walstore.NewReader(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("raft log store: open for recovery: %w", err)
	}
	defer r.Close()

	var entries []LogEntry
	for r.Scan() {
		data := r.Record().Data
		if len(data) == 0 {
			continue
		}
		switch data[0] {
		case logRecordTypeEntry:
			e, err := decodeLogEntry(data[1:])
			if err != nil {
				return nil, fmt.Errorf("raft log store: decode entry: %w", err)
			}
			entries = append(entries, e)
		case logRecordTypeTruncate:
			fromIndex, err := decodeTruncate(data[1:])
			if err != nil {
				return nil, fmt.Errorf("raft log store: decode truncate: %w", err)
			}
			cutAt := 0
			for cutAt < len(entries) && entries[cutAt].Index < fromIndex {
				cutAt++
			}
			entries = entries[:cutAt]
		default:
			return nil, fmt.Errorf("raft log store: unknown record type 0x%x", data[0])
		}
	}
	if err := r.Err(); err != nil && err != walstore.ErrTruncated {
		return nil, fmt.Errorf("raft log store: scan: %w", err)
	}
	return entries, nil
}

// Close closes the underlying WAL writer.
func (s *WALLogStore) Close() error {
	return s.w.Close()
}

// --- Encoding helpers -------------------------------------------------------

// encodeLogEntry serialises a LogEntry into a WAL payload.
// Format: [0x01][Index:8 LE][Term:8 LE][DataLen:4 LE][Data:N]
func encodeLogEntry(e LogEntry) []byte {
	dataLen := len(e.Data)
	buf := make([]byte, 1+8+8+4+dataLen)
	buf[0] = logRecordTypeEntry
	binary.LittleEndian.PutUint64(buf[1:9], uint64(e.Index))
	binary.LittleEndian.PutUint64(buf[9:17], uint64(e.Term))
	binary.LittleEndian.PutUint32(buf[17:21], uint32(dataLen))
	copy(buf[21:], e.Data)
	return buf
}

// decodeLogEntry deserialises the payload after the type byte.
func decodeLogEntry(data []byte) (LogEntry, error) {
	if len(data) < 20 {
		return LogEntry{}, fmt.Errorf("entry payload too short: %d bytes", len(data))
	}
	index := int64(binary.LittleEndian.Uint64(data[0:8]))
	term := int64(binary.LittleEndian.Uint64(data[8:16]))
	dataLen := int(binary.LittleEndian.Uint32(data[16:20]))
	if len(data) < 20+dataLen {
		return LogEntry{}, fmt.Errorf("entry data too short: need %d got %d", 20+dataLen, len(data))
	}
	var payload []byte
	if dataLen > 0 {
		payload = make([]byte, dataLen)
		copy(payload, data[20:20+dataLen])
	}
	return LogEntry{Index: index, Term: term, Data: payload}, nil
}

// encodeTruncate serialises a truncation marker.
// Format: [0x02][FromIndex:8 LE]
func encodeTruncate(fromIndex int64) []byte {
	buf := make([]byte, 9)
	buf[0] = logRecordTypeTruncate
	binary.LittleEndian.PutUint64(buf[1:9], uint64(fromIndex))
	return buf
}

// decodeTruncate deserialises the payload after the type byte.
func decodeTruncate(data []byte) (int64, error) {
	if len(data) < 8 {
		return 0, fmt.Errorf("truncate payload too short: %d bytes", len(data))
	}
	return int64(binary.LittleEndian.Uint64(data[0:8])), nil
}

// --- MemLogStore (tests) ----------------------------------------------------

// MemLogStore is an in-memory LogStore for tests that do not require
// disk persistence. It replays truncation in-place on TruncateSuffix.
// Safe for concurrent use.
type MemLogStore struct {
	mu      sync.Mutex
	entries []LogEntry
}

// NewMemLogStore creates an empty MemLogStore.
func NewMemLogStore() *MemLogStore { return &MemLogStore{} }

// Append appends entry to the in-memory log.
func (s *MemLogStore) Append(entry LogEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, entry)
	return nil
}

// TruncateSuffix removes all entries with Index >= fromIndex.
func (s *MemLogStore) TruncateSuffix(fromIndex int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutAt := 0
	for cutAt < len(s.entries) && s.entries[cutAt].Index < fromIndex {
		cutAt++
	}
	s.entries = s.entries[:cutAt]
	return nil
}

// LoadAll returns a copy of all in-memory entries.
func (s *MemLogStore) LoadAll() ([]LogEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]LogEntry, len(s.entries))
	copy(result, s.entries)
	return result, nil
}

// Close is a no-op.
func (s *MemLogStore) Close() error { return nil }
