package replication_test

import (
	"io"
	"os"
	"testing"

	"github.com/junyoung/core-x/internal/infrastructure/replication"
	"github.com/junyoung/core-x/internal/infrastructure/storage/wal"
)

func TestReceiver_AppendAndRecover(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	srcPath := srcDir + "/src.wal"
	sw, err := wal.NewWriter(wal.Config{Path: srcPath, SyncPolicy: wal.SyncNever})
	if err != nil {
		t.Fatal(err)
	}
	sw.Write([]byte("record-1"))
	sw.Write([]byte("record-2"))
	sw.Sync()
	sw.Close()

	srcFile, err := os.Open(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer srcFile.Close()

	var entries []replication.RawEntry
	offset := int64(0)
	for {
		rec, err := wal.ReadRecordAt(srcFile, offset)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("ReadRecordAt error: %v", err)
		}
		size := int64(wal.RecordHeaderSize) + int64(len(rec.Data)) + int64(wal.RecordChecksumSize)
		raw := make([]byte, size)
		srcFile.ReadAt(raw, offset)
		entries = append(entries, replication.RawEntry{Offset: offset, RawData: raw, RecordSize: size})
		offset += size
	}

	if len(entries) != 2 {
		t.Fatalf("expected 2 entries from source, got %d", len(entries))
	}

	dstPath := dstDir + "/dst.wal"
	recv, err := replication.NewReceiver(dstPath)
	if err != nil {
		t.Fatal(err)
	}
	defer recv.Close()

	for _, e := range entries {
		if err := recv.Append(e); err != nil {
			t.Fatalf("Append failed: %v", err)
		}
	}

	dstReader, err := wal.NewReader(dstPath)
	if err != nil {
		t.Fatal(err)
	}
	defer dstReader.Close()

	count := 0
	for dstReader.Scan() {
		count++
	}
	if dstReader.Err() != nil {
		t.Fatalf("dst WAL scan error: %v", dstReader.Err())
	}
	if count != 2 {
		t.Errorf("dst WAL has %d records, want 2", count)
	}
}
