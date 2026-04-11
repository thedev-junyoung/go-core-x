package wal_test

import (
	"os"
	"testing"
	"time"

	"github.com/junyoung/core-x/internal/infrastructure/storage/wal"
)

func TestCompactionNotify(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "wal-compaction-*.wal")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	w, err := wal.NewWriter(wal.Config{
		Path:       f.Name(),
		SyncPolicy: wal.SyncNever,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	ch1 := w.CompactionNotify()

	// Simulate a compaction swap.
	newFile, err := os.CreateTemp(t.TempDir(), "wal-new-*.wal")
	if err != nil {
		t.Fatal(err)
	}

	err = w.RunExclusiveSwap(func() (*os.File, int64, error) {
		return newFile, 0, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// ch1 should be closed after swap.
	select {
	case <-ch1:
		// expected
	case <-time.After(100 * time.Millisecond):
		t.Fatal("CompactionNotify channel was not closed after RunExclusiveSwap")
	}

	// A new channel should be returned after swap.
	ch2 := w.CompactionNotify()
	if ch1 == ch2 {
		t.Fatal("CompactionNotify should return a new channel after swap")
	}
}
