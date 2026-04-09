package kv

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/junyoung/core-x/internal/domain"
	"github.com/junyoung/core-x/internal/infrastructure/storage/wal"
)

// TestStore_WriteEventAndGet tests the basic write-read cycle.
func TestStore_WriteEventAndGet(t *testing.T) {
	tmpDir := t.TempDir()
	walPath := filepath.Join(tmpDir, "test.wal")

	// Create WAL writer
	writer, err := wal.NewWriter(wal.Config{
		Path:       walPath,
		SyncPolicy: wal.SyncNever, // Fast tests
	})
	if err != nil {
		t.Fatalf("failed to create wal writer: %v", err)
	}
	defer writer.Close()

	// Create KV store
	store, err := NewStore(writer, walPath, 100)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	// Write an event
	event := &domain.Event{
		ReceivedAt: time.Now(),
		Source:     "test-source",
		Payload:    "test-payload-data",
	}

	if err := store.WriteEvent(event); err != nil {
		t.Fatalf("WriteEvent failed: %v", err)
	}

	// Verify index entry was created
	if count := store.index.Len(); count != 1 {
		t.Errorf("expected index length 1, got %d", count)
	}

	// Get the event back
	retrieved, err := store.Get("test-source")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	// Verify content
	if retrieved.Source != event.Source {
		t.Errorf("source mismatch: expected %q, got %q", event.Source, retrieved.Source)
	}
	if retrieved.Payload != event.Payload {
		t.Errorf("payload mismatch: expected %q, got %q", event.Payload, retrieved.Payload)
	}

	// Verify timestamp is restored
	if retrieved.ReceivedAt.IsZero() {
		t.Error("ReceivedAt should not be zero")
	}
}

// TestStore_WriteEventAndGet_MultipleEvents tests overwrite semantics.
func TestStore_WriteEventAndGet_MultipleEvents(t *testing.T) {
	tmpDir := t.TempDir()
	walPath := filepath.Join(tmpDir, "test.wal")

	writer, err := wal.NewWriter(wal.Config{
		Path:       walPath,
		SyncPolicy: wal.SyncNever,
	})
	if err != nil {
		t.Fatalf("failed to create wal writer: %v", err)
	}
	defer writer.Close()

	store, err := NewStore(writer, walPath, 100)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	// Write two events from same source (latest should win)
	event1 := &domain.Event{
		ReceivedAt: time.Now(),
		Source:     "source-a",
		Payload:    "payload-1",
	}
	event2 := &domain.Event{
		ReceivedAt: time.Now(),
		Source:     "source-a", // Same source
		Payload:    "payload-2", // Different payload
	}

	if err := store.WriteEvent(event1); err != nil {
		t.Fatalf("WriteEvent #1 failed: %v", err)
	}
	if err := store.WriteEvent(event2); err != nil {
		t.Fatalf("WriteEvent #2 failed: %v", err)
	}

	// Should have 1 index entry (overwrite)
	if count := store.index.Len(); count != 1 {
		t.Errorf("expected index length 1, got %d", count)
	}

	// Get should return latest (payload-2)
	retrieved, err := store.Get("source-a")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	if retrieved.Payload != "payload-2" {
		t.Errorf("expected payload-2, got %q", retrieved.Payload)
	}
}

// TestStore_GetNotFound tests missing key behavior.
func TestStore_GetNotFound(t *testing.T) {
	tmpDir := t.TempDir()
	walPath := filepath.Join(tmpDir, "test.wal")

	writer, err := wal.NewWriter(wal.Config{
		Path:       walPath,
		SyncPolicy: wal.SyncNever,
	})
	if err != nil {
		t.Fatalf("failed to create wal writer: %v", err)
	}
	defer writer.Close()

	store, err := NewStore(writer, walPath, 100)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	// Try to get non-existent key
	_, err = store.Get("nonexistent")
	if err != ErrKeyNotFound {
		t.Errorf("expected ErrKeyNotFound, got %v", err)
	}
}

// TestStore_Recover tests WAL replay and index rebuild.
func TestStore_Recover(t *testing.T) {
	tmpDir := t.TempDir()
	walPath := filepath.Join(tmpDir, "test.wal")

	// Create initial WAL with some events
	writer, err := wal.NewWriter(wal.Config{
		Path:       walPath,
		SyncPolicy: wal.SyncNever,
	})
	if err != nil {
		t.Fatalf("failed to create wal writer: %v", err)
	}

	event1 := &domain.Event{
		ReceivedAt: time.Now(),
		Source:     "source-a",
		Payload:    "payload-a",
	}
	event2 := &domain.Event{
		ReceivedAt: time.Now(),
		Source:     "source-b",
		Payload:    "payload-b",
	}

	if err := writer.WriteEvent(event1); err != nil {
		t.Fatalf("WriteEvent #1 failed: %v", err)
	}
	if err := writer.WriteEvent(event2); err != nil {
		t.Fatalf("WriteEvent #2 failed: %v", err)
	}

	writer.Close()

	// Create new store (index is empty, but WAL has data)
	writer2, err := wal.NewWriter(wal.Config{
		Path:       walPath,
		SyncPolicy: wal.SyncNever,
	})
	if err != nil {
		t.Fatalf("failed to create wal writer: %v", err)
	}
	defer writer2.Close()

	store, err := NewStore(writer2, walPath, 100)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	// Index should be empty before recovery
	if count := store.index.Len(); count != 0 {
		t.Errorf("expected empty index before recovery, got %d entries", count)
	}

	// Run recovery
	count, err := store.Recover(nil)
	if err != nil {
		t.Fatalf("Recover failed: %v", err)
	}

	if count != 2 {
		t.Errorf("expected 2 recovered events, got %d", count)
	}

	// Index should now have 2 entries
	if indexLen := store.index.Len(); indexLen != 2 {
		t.Errorf("expected index length 2 after recovery, got %d", indexLen)
	}

	// Get should work for both sources
	retrieved1, err := store.Get("source-a")
	if err != nil {
		t.Fatalf("Get(source-a) failed: %v", err)
	}
	if retrieved1.Payload != "payload-a" {
		t.Errorf("source-a: expected payload-a, got %q", retrieved1.Payload)
	}

	retrieved2, err := store.Get("source-b")
	if err != nil {
		t.Fatalf("Get(source-b) failed: %v", err)
	}
	if retrieved2.Payload != "payload-b" {
		t.Errorf("source-b: expected payload-b, got %q", retrieved2.Payload)
	}
}

// TestStore_RecoverWithCallback tests recovery callback invocation.
func TestStore_RecoverWithCallback(t *testing.T) {
	tmpDir := t.TempDir()
	walPath := filepath.Join(tmpDir, "test.wal")

	// Create initial WAL
	writer, err := wal.NewWriter(wal.Config{
		Path:       walPath,
		SyncPolicy: wal.SyncNever,
	})
	if err != nil {
		t.Fatalf("failed to create wal writer: %v", err)
	}

	event := &domain.Event{
		ReceivedAt: time.Now(),
		Source:     "test",
		Payload:    "test-payload",
	}
	if err := writer.WriteEvent(event); err != nil {
		t.Fatalf("WriteEvent failed: %v", err)
	}
	writer.Close()

	// Create store and recover with callback
	writer2, err := wal.NewWriter(wal.Config{
		Path:       walPath,
		SyncPolicy: wal.SyncNever,
	})
	if err != nil {
		t.Fatalf("failed to create wal writer: %v", err)
	}
	defer writer2.Close()

	store, err := NewStore(writer2, walPath, 100)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	// Track callback invocations
	callbackCount := 0
	var callbackEvent *domain.Event

	count, err := store.Recover(func(e *domain.Event) {
		callbackCount++
		callbackEvent = e
	})

	if err != nil {
		t.Fatalf("Recover failed: %v", err)
	}

	if count != 1 {
		t.Errorf("expected 1 recovered event, got %d", count)
	}

	if callbackCount != 1 {
		t.Errorf("expected callback invoked 1 time, got %d", callbackCount)
	}

	if callbackEvent == nil {
		t.Fatal("callback should have received event")
	}

	if callbackEvent.Source != "test" {
		t.Errorf("callback event source: expected test, got %q", callbackEvent.Source)
	}
}

// TestStore_MaxKeys verifies maxKeys limit enforcement.
func TestStore_MaxKeys(t *testing.T) {
	tmpDir := t.TempDir()
	walPath := filepath.Join(tmpDir, "test.wal")

	writer, err := wal.NewWriter(wal.Config{
		Path:       walPath,
		SyncPolicy: wal.SyncNever,
	})
	if err != nil {
		t.Fatalf("failed to create wal writer: %v", err)
	}
	defer writer.Close()

	// Create store with maxKeys=1
	store, err := NewStore(writer, walPath, 1)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	// Write first event (should succeed)
	event1 := &domain.Event{
		ReceivedAt: time.Now(),
		Source:     "source-a",
		Payload:    "payload-a",
	}
	if err := store.WriteEvent(event1); err != nil {
		t.Fatalf("WriteEvent #1 failed: %v", err)
	}

	// Write second event with different source (should fail with ErrIndexFull)
	event2 := &domain.Event{
		ReceivedAt: time.Now(),
		Source:     "source-b",
		Payload:    "payload-b",
	}
	err = store.WriteEvent(event2)
	if err != ErrIndexFull {
		t.Errorf("expected ErrIndexFull, got %v", err)
	}

	// Write to same source should succeed (overwrite, not new entry)
	event3 := &domain.Event{
		ReceivedAt: time.Now(),
		Source:     "source-a", // Same as event1
		Payload:    "payload-a-updated",
	}
	if err := store.WriteEvent(event3); err != nil {
		t.Fatalf("WriteEvent #3 (overwrite) failed: %v", err)
	}

	// Verify only 1 entry in index
	if count := store.index.Len(); count != 1 {
		t.Errorf("expected 1 index entry, got %d", count)
	}
}

// TestStore_WALWriterInterface verifies Store implements WALWriter interface.
func TestStore_WALWriterInterface(t *testing.T) {
	tmpDir := t.TempDir()
	walPath := filepath.Join(tmpDir, "test.wal")

	writer, err := wal.NewWriter(wal.Config{
		Path:       walPath,
		SyncPolicy: wal.SyncNever,
	})
	if err != nil {
		t.Fatalf("failed to create wal writer: %v", err)
	}
	defer writer.Close()

	store, err := NewStore(writer, walPath, 100)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	// Compile-time check: Store.WriteEvent must implement WALWriter.WriteEvent
	// This is a compile-time assertion; if Store doesn't implement WriteEvent(e *domain.Event) error,
	// this line will fail to compile.
	//
	// Note: We can't use var _ pattern at package level for interface.
	// Instead, we just call WriteEvent and verify it works.
	event := &domain.Event{
		ReceivedAt: time.Now(),
		Source:     "test",
		Payload:    "test",
	}
	if err := store.WriteEvent(event); err != nil {
		t.Fatalf("Store.WriteEvent failed (interface impl broken): %v", err)
	}
}

// TestStore_NoWALFile tests behavior when WAL file doesn't exist yet.
func TestStore_NoWALFile(t *testing.T) {
	tmpDir := t.TempDir()
	walPath := filepath.Join(tmpDir, "nonexistent.wal")

	// Note: NewStore will try to open the file, and may fail
	// if the WAL writer hasn't created it yet. This test documents current behavior.

	// First, create writer to create the file
	writer, err := wal.NewWriter(wal.Config{
		Path:       walPath,
		SyncPolicy: wal.SyncNever,
	})
	if err != nil {
		t.Fatalf("failed to create wal writer: %v", err)
	}
	defer writer.Close()

	// Now store can be created
	store, err := NewStore(writer, walPath, 100)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	// Recovery on empty WAL should return 0 events, no error
	count, err := store.Recover(nil)
	if err != nil {
		t.Fatalf("Recover on empty WAL failed: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 recovered events from empty WAL, got %d", count)
	}
}
