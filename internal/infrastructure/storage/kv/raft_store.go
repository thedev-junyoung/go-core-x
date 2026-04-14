// raft_store.go — Raft KV write/recover path for kv.Store.
//
// This file adds Raft-specific methods to kv.Store without modifying the
// existing Phase 1–2 ingestion path (WriteEvent / Get / Recover).
//
// Invariant (INV-1, ADR-016):
//
//	WriteKV(key, value, raftIndex) success
//	  BEFORE
//	caller advances lastApplied
//
// The caller (KVStateMachine.apply) is responsible for ordering; this file
// guarantees that both the data record and the meta checkpoint record are
// durably appended to the WAL before returning success.
//
// WAL file:
//
//	The kv.Store instance used for Raft KV (data/kv.wal) is separate from
//	the ingestion events.wal. SyncPolicy must be SyncInterval(100ms) per ADR-016.
package kv

import (
	"fmt"
	"os"
	"sync/atomic"

	"github.com/junyoung/core-x/internal/infrastructure/storage/wal"
)

// WriteKV appends a "set" record for (key, value, raftIndex) to the KV WAL
// and updates the in-memory index. A meta checkpoint record
// (__raft_last_applied__) is written in the same operation.
//
// The caller must advance lastApplied only AFTER this function returns nil.
//
// Returns ErrReservedKey if key == MetaKeyLastApplied.
func (s *Store) WriteKV(key, value string, raftIndex int64) error {
	if key == MetaKeyLastApplied {
		return ErrReservedKey
	}

	var scratch [0]byte
	dataBuf := encodeKVSet(scratch[:0], key, value, raftIndex)

	s.mu.Lock()
	defer s.mu.Unlock()

	// Write data record; get offset for index update.
	offset, err := s.writer.WriteOffset(dataBuf)
	if err != nil {
		return fmt.Errorf("kv: WriteKV data record: %w", err)
	}

	// Update in-memory index.
	if idxErr := s.index.Set(key, offset); idxErr != nil {
		// Index full — WAL record is written but will be re-indexed on recovery.
		// Still write the meta checkpoint so we don't re-apply this entry.
		_ = s.writeMetaLocked(raftIndex)
		return fmt.Errorf("kv: WriteKV index update: %w", idxErr)
	}

	// Write meta checkpoint.
	if metaErr := s.writeMetaLocked(raftIndex); metaErr != nil {
		return fmt.Errorf("kv: WriteKV meta record: %w", metaErr)
	}

	atomic.StoreInt64(&s.raftLastApplied, raftIndex)
	return nil
}

// DeleteKV appends a delete tombstone for key and a meta checkpoint record.
//
// Returns ErrReservedKey if key == MetaKeyLastApplied.
func (s *Store) DeleteKV(key string, raftIndex int64) error {
	if key == MetaKeyLastApplied {
		return ErrReservedKey
	}

	var scratch [0]byte
	delBuf := encodeKVDel(scratch[:0], key, raftIndex)

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.writer.WriteOffset(delBuf); err != nil {
		return fmt.Errorf("kv: DeleteKV del record: %w", err)
	}

	s.index.Delete(key)

	if metaErr := s.writeMetaLocked(raftIndex); metaErr != nil {
		return fmt.Errorf("kv: DeleteKV meta record: %w", metaErr)
	}

	atomic.StoreInt64(&s.raftLastApplied, raftIndex)
	return nil
}

// GetKV retrieves the current string value for key from the WAL-backed index.
//
// Returns ("", false, nil) when the key does not exist.
// Returns a non-nil error only on WAL read or decode failure.
func (s *Store) GetKV(key string) (string, bool, error) {
	s.mu.RLock()
	idx := s.index
	rf := s.readFile
	s.mu.RUnlock()

	offset, found := idx.Get(key)
	if !found {
		return "", false, nil
	}

	record, err := wal.ReadRecordAt(rf, offset)
	if err != nil {
		return "", false, fmt.Errorf("kv: GetKV read at offset %d: %w", offset, err)
	}

	rec, err := decodeKVRecord(record.Data)
	if err != nil {
		return "", false, fmt.Errorf("kv: GetKV decode at offset %d: %w", offset, err)
	}

	return rec.Value, true, nil
}

// RecoverKV replays the KV WAL to rebuild the in-memory index and determine
// the last Raft log index applied to this store.
//
// Must be called once at startup before any WriteKV/DeleteKV calls.
//
// Returns (lastAppliedIndex, nil) on success.
// Returns (0, nil) on a fresh start (no WAL file yet).
// ErrTruncated in the WAL is treated as normal (partial last record); the
// recovered state up to that point is returned.
// Any other WAL error is returned as a fatal error.
func (s *Store) RecoverKV() (int64, error) {
	if _, err := os.Stat(s.walPath); os.IsNotExist(err) {
		return 0, nil
	}

	reader, err := wal.NewReader(s.walPath)
	if err != nil {
		return 0, fmt.Errorf("kv: RecoverKV open reader: %w", err)
	}
	defer reader.Close()

	var lastApplied int64
	offset := int64(0)

	for reader.Scan() {
		record := reader.Record()
		recordSize := int64(wal.RecordHeaderSize) + int64(len(record.Data)) + int64(wal.RecordChecksumSize)

		rec, decErr := decodeKVRecord(record.Data)
		if decErr != nil {
			return lastApplied, fmt.Errorf("kv: RecoverKV decode at offset %d: %w", offset, decErr)
		}

		switch rec.Type {
		case kvRecordTypeSet:
			if idxErr := s.index.Set(rec.Key, offset); idxErr != nil {
				return lastApplied, fmt.Errorf("kv: RecoverKV index full at offset %d: %w", offset, idxErr)
			}
		case kvRecordTypeDel:
			s.index.Delete(rec.Key)
		case kvRecordTypeMeta:
			if rec.Key == MetaKeyLastApplied {
				lastApplied = rec.RaftIndex
			}
		}

		offset += recordSize
	}

	if readErr := reader.Err(); readErr != nil {
		if readErr == wal.ErrTruncated {
			// Normal crash truncation — return whatever was recovered.
			atomic.StoreInt64(&s.raftLastApplied, lastApplied)
			return lastApplied, nil
		}
		return lastApplied, fmt.Errorf("kv: RecoverKV WAL error: %w", readErr)
	}

	atomic.StoreInt64(&s.raftLastApplied, lastApplied)
	return lastApplied, nil
}

// --- private helpers ---------------------------------------------------------

// writeMetaLocked appends a __raft_last_applied__ meta checkpoint record.
// Caller must hold s.mu (Lock, not RLock).
func (s *Store) writeMetaLocked(raftIndex int64) error {
	var scratch [0]byte
	metaBuf := encodeKVMeta(scratch[:0], MetaKeyLastApplied, raftIndex)
	if err := s.writer.Write(metaBuf); err != nil {
		return err
	}
	return nil
}
