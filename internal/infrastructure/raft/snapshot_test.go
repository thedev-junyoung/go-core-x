package raft

// Phase 9a snapshot tests: FileSnapshotStore round-trip, KVStateMachine
// TakeSnapshot/RestoreSnapshot, WALLogStore CompactPrefix, and RaftNode
// recoverFromSnapshot.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ---- FileSnapshotStore tests -----------------------------------------------

func TestFileSnapshotStore_SaveAndLoad(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFileSnapshotStore(dir)
	if err != nil {
		t.Fatalf("NewFileSnapshotStore: %v", err)
	}

	meta := SnapshotMeta{Index: 42, Term: 3, CreatedAt: time.Now()}
	data := SnapshotData{KV: map[string]string{"foo": "bar", "baz": "qux"}}

	if err := store.Save(meta, data); err != nil {
		t.Fatalf("Save: %v", err)
	}

	gotMeta, gotData, err := store.Load(42)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if gotMeta.Index != 42 || gotMeta.Term != 3 {
		t.Errorf("meta mismatch: got index=%d term=%d", gotMeta.Index, gotMeta.Term)
	}
	if len(gotData.KV) != 2 || gotData.KV["foo"] != "bar" || gotData.KV["baz"] != "qux" {
		t.Errorf("data mismatch: %v", gotData.KV)
	}
}

func TestFileSnapshotStore_LoadNotFound(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewFileSnapshotStore(dir)

	_, _, err := store.Load(99)
	if err != ErrSnapshotNotFound {
		t.Errorf("expected ErrSnapshotNotFound, got %v", err)
	}
}

func TestFileSnapshotStore_LatestEmpty(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewFileSnapshotStore(dir)

	_, err := store.Latest()
	if err != ErrSnapshotNotFound {
		t.Errorf("expected ErrSnapshotNotFound on empty store, got %v", err)
	}
}

func TestFileSnapshotStore_Latest(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewFileSnapshotStore(dir)

	for _, idx := range []int64{10, 30, 20} {
		m := SnapshotMeta{Index: idx, Term: 1}
		d := SnapshotData{KV: map[string]string{"k": "v"}}
		if err := store.Save(m, d); err != nil {
			t.Fatalf("Save index=%d: %v", idx, err)
		}
	}

	latest, err := store.Latest()
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if latest.Index != 30 {
		t.Errorf("expected latest index=30, got %d", latest.Index)
	}
}

func TestFileSnapshotStore_Prune(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewFileSnapshotStore(dir)

	for _, idx := range []int64{10, 20, 30, 40, 50} {
		m := SnapshotMeta{Index: idx, Term: 1}
		d := SnapshotData{KV: map[string]string{"k": "v"}}
		if err := store.Save(m, d); err != nil {
			t.Fatalf("Save index=%d: %v", idx, err)
		}
	}

	if err := store.Prune(2); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	metas, err := store.List()
	if err != nil {
		t.Fatalf("List after prune: %v", err)
	}
	if len(metas) != 2 {
		t.Errorf("expected 2 snapshots after prune, got %d", len(metas))
	}
	// Newest 2 should be retained.
	if metas[0].Index != 50 || metas[1].Index != 40 {
		t.Errorf("wrong snapshots retained: %v", metas)
	}
}

func TestFileSnapshotStore_CorruptedCRC(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewFileSnapshotStore(dir)

	meta := SnapshotMeta{Index: 1, Term: 1}
	data := SnapshotData{KV: map[string]string{"x": "y"}}
	if err := store.Save(meta, data); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Corrupt the file by flipping a byte in the middle of the body.
	path := filepath.Join(dir, "snapshot-1-1.snap")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(raw) > snapshotHeaderSize {
		raw[snapshotHeaderSize] ^= 0xFF
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, _, err = store.Load(1)
	if err != ErrCorruptedSnapshot {
		t.Errorf("expected ErrCorruptedSnapshot, got %v", err)
	}
}

func TestFileSnapshotStore_TmpCleanupOnNew(t *testing.T) {
	dir := t.TempDir()
	// Leave a stray .tmp file to simulate a previous crashed Save.
	strayPath := filepath.Join(dir, "snapshot-5-1.snap.tmp")
	if err := os.WriteFile(strayPath, []byte("stale"), 0600); err != nil {
		t.Fatalf("create stray: %v", err)
	}

	_, err := NewFileSnapshotStore(dir)
	if err != nil {
		t.Fatalf("NewFileSnapshotStore: %v", err)
	}

	if _, statErr := os.Stat(strayPath); !os.IsNotExist(statErr) {
		t.Errorf("stray .tmp was not cleaned up")
	}
}

// ---- KVStateMachine Snapshotable tests -------------------------------------

func TestKVStateMachine_TakeRestoreSnapshot(t *testing.T) {
	sm := NewKVStateMachine(nil)

	sm.apply(LogEntry{Index: 1, Term: 1, Data: mustMarshalKVCmd(t, "set", "a", "1")})
	sm.apply(LogEntry{Index: 2, Term: 1, Data: mustMarshalKVCmd(t, "set", "b", "2")})

	snap, idx, err := sm.TakeSnapshot()
	if err != nil {
		t.Fatalf("TakeSnapshot: %v", err)
	}
	if idx != 2 {
		t.Errorf("expected lastApplied=2, got %d", idx)
	}
	if snap.KV["a"] != "1" || snap.KV["b"] != "2" {
		t.Errorf("snapshot KV mismatch: %v", snap.KV)
	}

	sm2 := NewKVStateMachine(nil)
	if err := sm2.RestoreSnapshot(snap, idx); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}
	if sm2.LastApplied() != 2 {
		t.Errorf("lastApplied after restore: want 2, got %d", sm2.LastApplied())
	}
	v, ok := sm2.Get("a")
	if !ok || v != "1" {
		t.Errorf("Get(a) after restore: got %q %v", v, ok)
	}
}

func TestKVStateMachine_SnapshotCOWIsolation(t *testing.T) {
	sm := NewKVStateMachine(nil)
	sm.apply(LogEntry{Index: 1, Term: 1, Data: mustMarshalKVCmd(t, "set", "key", "original")})

	snap, _, err := sm.TakeSnapshot()
	if err != nil {
		t.Fatalf("TakeSnapshot: %v", err)
	}

	// Mutate sm after snapshot — snapshot must not reflect the mutation (COW).
	sm.apply(LogEntry{Index: 2, Term: 1, Data: mustMarshalKVCmd(t, "set", "key", "mutated")})

	if snap.KV["key"] != "original" {
		t.Errorf("COW violation: snapshot sees %q, want %q", snap.KV["key"], "original")
	}
}

// ---- WALLogStore CompactPrefix test ----------------------------------------

func TestWALLogStore_CompactPrefix(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "raft.wal")

	s, err := NewWALLogStore(path)
	if err != nil {
		t.Fatalf("NewWALLogStore: %v", err)
	}
	defer s.Close()

	for i := int64(1); i <= 5; i++ {
		if err := s.Append(LogEntry{Index: i, Term: 1, Data: []byte("data")}); err != nil {
			t.Fatalf("Append index=%d: %v", i, err)
		}
	}

	if err := s.CompactPrefix(3); err != nil {
		t.Fatalf("CompactPrefix: %v", err)
	}

	entries, err := s.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll after compact: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("expected 2 entries after compact, got %d", len(entries))
	}
	if entries[0].Index != 4 || entries[1].Index != 5 {
		t.Errorf("wrong entries after compact: %v", entries)
	}

	// Subsequent appends must work after compaction.
	if err := s.Append(LogEntry{Index: 6, Term: 1, Data: []byte("new")}); err != nil {
		t.Fatalf("Append after compact: %v", err)
	}
	entries2, err := s.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll after new append: %v", err)
	}
	if len(entries2) != 3 || entries2[2].Index != 6 {
		t.Errorf("entries after post-compact append: %v", entries2)
	}
}

// ---- MemLogStore CompactPrefix test ----------------------------------------

func TestMemLogStore_CompactPrefix(t *testing.T) {
	s := NewMemLogStore()
	for i := int64(1); i <= 5; i++ {
		_ = s.Append(LogEntry{Index: i, Term: 1})
	}

	if err := s.CompactPrefix(3); err != nil {
		t.Fatalf("CompactPrefix: %v", err)
	}

	entries, _ := s.LoadAll()
	if len(entries) != 2 || entries[0].Index != 4 {
		t.Errorf("CompactPrefix result: %v", entries)
	}
}

// ---- RaftNode snapshot integration tests -----------------------------------

func TestRaftNode_MaybeSnapshot_Threshold(t *testing.T) {
	dir := t.TempDir()
	snapStore, err := NewFileSnapshotStore(dir)
	if err != nil {
		t.Fatalf("NewFileSnapshotStore: %v", err)
	}

	logStore := NewMemLogStore()
	n := NewRaftNode("node1", nil, nil, logStore)
	sm := NewKVStateMachine(nil)
	n.SetStateMachine(sm)
	n.SetSnapshotStore(snapStore, SnapshotConfig{
		Threshold:   3,
		RetainCount: 2,
	})

	// Apply 4 entries (threshold=3, so 4 >= 3 should trigger snapshot).
	for i := int64(1); i <= 4; i++ {
		sm.apply(LogEntry{Index: i, Term: 1, Data: mustMarshalKVCmd(t, "set", "k", "v")})
		_ = logStore.Append(LogEntry{Index: i, Term: 1})
		n.mu.Lock()
		n.log = append(n.log, LogEntry{Index: i, Term: 1})
		n.mu.Unlock()
	}

	n.maybeSnapshot()

	// Wait for background goroutine.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if n.snapshotInProgress.Load() == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n.snapshotInProgress.Load() != 0 {
		t.Fatal("snapshot goroutine did not complete in time")
	}

	latest, err := snapStore.Latest()
	if err != nil {
		t.Fatalf("Latest after maybeSnapshot: %v", err)
	}
	if latest.Index != 4 {
		t.Errorf("expected snapshot at index 4, got %d", latest.Index)
	}

	n.mu.Lock()
	logLen := len(n.log)
	snapIdx := n.snapshotIndex
	n.mu.Unlock()

	if logLen != 0 {
		t.Errorf("expected empty log after compact, got len=%d", logLen)
	}
	if snapIdx != 4 {
		t.Errorf("expected snapshotIndex=4, got %d", snapIdx)
	}
}

func TestRaftNode_MaybeSnapshot_BelowThreshold(t *testing.T) {
	dir := t.TempDir()
	snapStore, _ := NewFileSnapshotStore(dir)

	n := NewRaftNode("node1", nil, nil, NewMemLogStore())
	sm := NewKVStateMachine(nil)
	n.SetStateMachine(sm)
	n.SetSnapshotStore(snapStore, SnapshotConfig{Threshold: 10, RetainCount: 2})

	// Apply only 3 entries (below threshold of 10).
	for i := int64(1); i <= 3; i++ {
		sm.apply(LogEntry{Index: i, Term: 1, Data: mustMarshalKVCmd(t, "set", "k", "v")})
	}

	n.maybeSnapshot()
	time.Sleep(50 * time.Millisecond)

	_, err := snapStore.Latest()
	if err != ErrSnapshotNotFound {
		t.Errorf("expected no snapshot below threshold, got err=%v", err)
	}
}

func TestRaftNode_RecoverFromSnapshot(t *testing.T) {
	dir := t.TempDir()
	snapStore, _ := NewFileSnapshotStore(dir)

	// Save a snapshot at index=5.
	snapData := SnapshotData{KV: map[string]string{"restored": "yes"}}
	snapMeta := SnapshotMeta{Index: 5, Term: 2}
	if err := snapStore.Save(snapMeta, snapData); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// logStore contains entries 3-7 (3,4,5 already covered by snapshot).
	logStore := NewMemLogStore()
	for i := int64(3); i <= 7; i++ {
		_ = logStore.Append(LogEntry{Index: i, Term: 2})
	}

	n := NewRaftNode("node1", nil, nil, logStore)
	sm := NewKVStateMachine(nil)
	n.SetStateMachine(sm)
	n.SetSnapshotStore(snapStore, SnapshotConfig{Threshold: 100, RetainCount: 2})

	snapshotIdx := n.RecoverFromSnapshot()

	if snapshotIdx != 5 {
		t.Errorf("RecoverFromSnapshot: want 5, got %d", snapshotIdx)
	}
	if n.snapshotIndex != 5 || n.snapshotTerm != 2 {
		t.Errorf("snapshotIndex/Term: want 5/2, got %d/%d", n.snapshotIndex, n.snapshotTerm)
	}

	v, ok := sm.Get("restored")
	if !ok || v != "yes" {
		t.Errorf("sm.Get(restored) = %q %v; want yes true", v, ok)
	}

	// n.log should contain only entries with Index > 5 (i.e. 6 and 7).
	if len(n.log) != 2 {
		t.Errorf("expected 2 log entries after recover, got %d: %v", len(n.log), n.log)
	}
	if n.log[0].Index != 6 || n.log[1].Index != 7 {
		t.Errorf("wrong log entries after recover: %v", n.log)
	}
}

func TestRaftNode_LastEntryAfterSnapshot(t *testing.T) {
	n := NewRaftNode("node1", nil, nil, nil)
	n.snapshotIndex = 50
	n.snapshotTerm = 3

	n.mu.Lock()
	idx, term := n.lastEntry()
	n.mu.Unlock()

	if idx != 50 || term != 3 {
		t.Errorf("lastEntry() after snapshot: want (50,3), got (%d,%d)", idx, term)
	}
}

func TestRaftNode_EntryAtAfterCompaction(t *testing.T) {
	n := NewRaftNode("node1", nil, nil, nil)

	n.mu.Lock()
	n.snapshotIndex = 100
	n.snapshotTerm = 2
	n.log = []LogEntry{
		{Index: 101, Term: 2},
		{Index: 102, Term: 2},
		{Index: 103, Term: 3},
	}

	_, ok100 := n.entryAt(100) // compacted into snapshot
	e101, ok101 := n.entryAt(101)
	e103, ok103 := n.entryAt(103)
	_, ok104 := n.entryAt(104) // beyond log
	n.mu.Unlock()

	if ok100 {
		t.Errorf("entryAt(100) should be false (covered by snapshot)")
	}
	if !ok101 || e101.Index != 101 {
		t.Errorf("entryAt(101) = (%v, %v); want ({Index:101}, true)", e101, ok101)
	}
	if !ok103 || e103.Index != 103 {
		t.Errorf("entryAt(103) = (%v, %v); want ({Index:103}, true)", e103, ok103)
	}
	if ok104 {
		t.Errorf("entryAt(104) should be false")
	}
}

// ---- Benchmarks ------------------------------------------------------------

func BenchmarkFileSnapshotStore_Save(b *testing.B) {
	dir := b.TempDir()
	store, _ := NewFileSnapshotStore(dir)

	kv := make(map[string]string, 100)
	for i := 0; i < 100; i++ {
		kv[string(rune('a'+i%26))+string(rune('0'+i/26))] = "value"
	}
	data := SnapshotData{KV: kv}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		meta := SnapshotMeta{Index: int64(i + 1), Term: 1}
		_ = store.Save(meta, data)
	}
}

func BenchmarkEncodeDecodeSnapshot(b *testing.B) {
	kv := make(map[string]string, 100)
	for i := 0; i < 100; i++ {
		kv["key"+string(rune('0'+i%10))] = "value"
	}
	data := SnapshotData{KV: kv}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		raw, _, err := encodeSnapshot(int64(i), 1, data)
		if err != nil {
			b.Fatalf("encode: %v", err)
		}
		if _, _, err := decodeSnapshot(raw); err != nil {
			b.Fatalf("decode: %v", err)
		}
	}
}

// ---- helpers ---------------------------------------------------------------

// mustMarshalKVCmd creates a JSON-encoded RaftKVCommand for use in tests.
func mustMarshalKVCmd(t *testing.T, op, key, value string) []byte {
	t.Helper()
	b, err := json.Marshal(RaftKVCommand{Op: op, Key: key, Value: value})
	if err != nil {
		t.Fatalf("json.Marshal RaftKVCommand: %v", err)
	}
	return b
}
