// Package kv — Compact rewrites the WAL retaining only the latest record per key.
package kv

import (
	"fmt"
	"os"

	"github.com/junyoung/core-x/internal/infrastructure/storage/wal"
)

// Compact rewrites the WAL retaining only the latest record per key.
//
// Strategy: stop-the-world compaction.
//   - Acquires writer lock for the entire duration (blocks concurrent writes).
//   - Pause duration is O(unique keys × average record size).
//   - Production improvement: background copy-then-swap — write compacted file
//     without the lock, then lock only for the final rename + pointer swap.
//
// Procedure (all steps execute under writer lock):
//  1. Snapshot current index → live key set with their WAL offsets.
//  2. Write each key's latest record to walPath+".compact" (temp file).
//  3. fsync the temp file.
//  4. os.Rename(tempPath, walPath) — atomic on POSIX filesystems.
//  5. Swap Store.readFile to the new file (old handle is now stale).
//  6. Rebuild Store.index with compacted offsets.
//  7. Return new writer file handle to RunExclusiveSwap (updates writer internals).
//
// Crash safety:
//   - Before rename: original WAL is untouched. A crash leaves a .compact orphan
//     that is removed at the start of the next Compact() call.
//   - After rename: compacted WAL is in place. On next startup, Recover() replays
//     it and rebuilds the index correctly.
//
// Returns nil if the WAL has no live keys (no-op creates an empty compacted file,
// which is semantically correct).
func (s *Store) Compact() error {
	tempPath := s.walPath + ".compact"

	// Remove leftover temp file from a previous failed compaction.
	_ = os.Remove(tempPath)

	return s.writer.RunExclusiveSwap(func() (*os.File, int64, error) {
		// Step 1: Snapshot current index (safe — no concurrent writes, lock held).
		snapshot := s.index.Snapshot()

		// Step 2–3: Write compacted WAL to temp file.
		newOffsets, err := writeCompactedWAL(tempPath, snapshot, s.readFile)
		if err != nil {
			_ = os.Remove(tempPath)
			return nil, 0, err
		}

		// Measure compacted size before rename.
		fi, err := os.Stat(tempPath)
		if err != nil {
			_ = os.Remove(tempPath)
			return nil, 0, fmt.Errorf("kv: compact: stat temp file: %w", err)
		}
		compactedSize := fi.Size()

		// Step 4: Atomic rename — replaces walPath in one syscall.
		if err := os.Rename(tempPath, s.walPath); err != nil {
			_ = os.Remove(tempPath)
			return nil, 0, fmt.Errorf("kv: compact: rename: %w", err)
		}

		// Step 5: Reopen readFile (old handle pointed to the pre-compaction inode).
		if err := s.readFile.Close(); err != nil {
			return nil, 0, fmt.Errorf("kv: compact: close old readFile: %w", err)
		}
		newReadFile, err := os.Open(s.walPath)
		if err != nil {
			return nil, 0, fmt.Errorf("kv: compact: open readFile: %w", err)
		}

		// Step 6: Rebuild index with compacted offsets.
		// Hold mu.Lock so Get() cannot observe a partially updated (index, readFile) pair.
		newIndex := NewHashIndex(s.index.maxKeys)
		for key, offset := range newOffsets {
			if err := newIndex.Set(key, offset); err != nil {
				_ = newReadFile.Close()
				return nil, 0, fmt.Errorf("kv: compact: rebuild index: %w", err)
			}
		}

		s.mu.Lock()
		s.readFile = newReadFile
		s.index = newIndex
		s.mu.Unlock()

		// Step 7: Open new writer file handle (O_APPEND|O_WRONLY).
		newWriterFile, err := os.OpenFile(s.walPath, os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return nil, 0, fmt.Errorf("kv: compact: open writer file: %w", err)
		}

		return newWriterFile, compactedSize, nil
	})
}

// writeCompactedWAL writes the latest record for each key in snapshot to destPath.
//
// Reads each record from readFile at the offset stored in snapshot (the pre-compaction
// WAL), then writes it to a new WAL at destPath. Returns a map of key → new offset
// in the compacted file.
//
// fsync is called once after all records are written (batched fsync).
func writeCompactedWAL(destPath string, snapshot map[string]int64, readFile *os.File) (map[string]int64, error) {
	w, err := wal.NewWriter(wal.Config{
		Path:       destPath,
		SyncPolicy: wal.SyncNever, // explicit Sync() at end of function
	})
	if err != nil {
		return nil, fmt.Errorf("kv: compact: create temp writer: %w", err)
	}
	defer w.Close()

	newOffsets := make(map[string]int64, len(snapshot))

	for key, oldOffset := range snapshot {
		record, err := wal.ReadRecordAt(readFile, oldOffset)
		if err != nil {
			return nil, fmt.Errorf("kv: compact: read key %q at offset %d: %w", key, oldOffset, err)
		}

		event, err := wal.DecodeEvent(record.Data)
		if err != nil {
			return nil, fmt.Errorf("kv: compact: decode key %q: %w", key, err)
		}
		event.ReceivedAt = record.Timestamp

		newOffset, err := w.WriteEventOffset(event)
		if err != nil {
			return nil, fmt.Errorf("kv: compact: write key %q: %w", key, err)
		}
		newOffsets[key] = newOffset
	}

	// Single fsync after all records — batched for efficiency.
	if err := w.Sync(); err != nil {
		return nil, fmt.Errorf("kv: compact: sync: %w", err)
	}

	return newOffsets, nil
}
