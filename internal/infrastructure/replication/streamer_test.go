package replication_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/junyoung/core-x/internal/infrastructure/replication"
	"github.com/junyoung/core-x/internal/infrastructure/storage/wal"
)

func TestStreamer_SendsRecords(t *testing.T) {
	dir := t.TempDir()
	walPath := dir + "/test.wal"

	w, err := wal.NewWriter(wal.Config{Path: walPath, SyncPolicy: wal.SyncNever})
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("hello"))
	w.Write([]byte("world"))
	w.Write([]byte("bye"))
	w.Sync()
	w.Close()

	f, err := os.Open(walPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	lag := replication.NewReplicationLag()
	streamer := replication.NewStreamer(f, w.CompactionNotify, lag, 5*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	var entries []replication.RawEntry
	err = streamer.Stream(ctx, 0, func(e replication.RawEntry) error {
		entries = append(entries, e)
		if len(entries) == 3 {
			cancel()
		}
		return nil
	})

	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d (err=%v)", len(entries), err)
	}
	for i := 1; i < len(entries); i++ {
		if entries[i].Offset <= entries[i-1].Offset {
			t.Errorf("entries[%d].Offset=%d not > entries[%d].Offset=%d",
				i, entries[i].Offset, i-1, entries[i-1].Offset)
		}
	}
}
