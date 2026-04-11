package replication

import (
	"context"
	"io"
	"os"
	"time"

	"github.com/junyoung/core-x/internal/infrastructure/storage/wal"
)

// RawEntry is a single raw WAL record to be sent to a replica.
type RawEntry struct {
	Offset     int64
	RawData    []byte
	RecordSize int64
}

// Streamer tails a WAL file and delivers RawEntry values to a callback.
type Streamer struct {
	walFile          *os.File
	compactionNotify func() <-chan struct{}
	lag              *ReplicationLag
	pollInterval     time.Duration
}

// NewStreamer creates a Streamer for the given open WAL file.
func NewStreamer(walFile *os.File, compactionNotify func() <-chan struct{}, lag *ReplicationLag, pollInterval time.Duration) *Streamer {
	return &Streamer{
		walFile:          walFile,
		compactionNotify: compactionNotify,
		lag:              lag,
		pollInterval:     pollInterval,
	}
}

// Stream reads WAL records starting at startOffset and calls fn for each one.
// On EOF it polls until new data arrives or ctx is done.
// On compaction signal, reopens the WAL file from offset 0.
func (s *Streamer) Stream(ctx context.Context, startOffset int64, fn func(RawEntry) error) error {
	f := s.walFile
	offset := startOffset
	compactionCh := s.compactionNotify()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-compactionCh:
			newF, err := os.Open(f.Name())
			if err != nil {
				return err
			}
			if f != s.walFile {
				f.Close()
			}
			f = newF
			offset = 0
			compactionCh = s.compactionNotify()
			continue
		default:
		}

		rec, err := wal.ReadRecordAt(f, offset)
		if err == io.EOF {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-compactionCh:
				newF, openErr := os.Open(f.Name())
				if openErr != nil {
					return openErr
				}
				if f != s.walFile {
					f.Close()
				}
				f = newF
				offset = 0
				compactionCh = s.compactionNotify()
			case <-time.After(s.pollInterval):
				// retry
			}
			continue
		}
		if err != nil {
			return err
		}

		recordSize := int64(wal.RecordHeaderSize) + int64(len(rec.Data)) + int64(wal.RecordChecksumSize)

		raw := make([]byte, recordSize)
		if _, err := f.ReadAt(raw, offset); err != nil {
			return err
		}

		entry := RawEntry{
			Offset:     offset,
			RawData:    raw,
			RecordSize: recordSize,
		}

		if err := fn(entry); err != nil {
			return err
		}

		offset += recordSize
		s.lag.UpdatePrimary(offset)
	}
}
