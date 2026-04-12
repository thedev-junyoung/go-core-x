package raft_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/junyoung/core-x/internal/infrastructure/raft"
	pb "github.com/junyoung/core-x/proto/pb"
)

// newTestWALLogStore creates a WALLogStore backed by a temp file and
// returns the store and a cleanup function.
func newTestWALLogStore(t *testing.T) (*raft.WALLogStore, func()) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "raft_log.wal")
	s, err := raft.NewWALLogStore(path)
	if err != nil {
		t.Fatalf("NewWALLogStore: %v", err)
	}
	return s, func() { s.Close() }
}

// TestWALLogStore_AppendAndLoadAll verifies that appended entries are
// recovered correctly after the store is closed and reopened.
func TestWALLogStore_AppendAndLoadAll(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "raft_log.wal")

	entries := []raft.LogEntry{
		{Index: 1, Term: 1, Data: []byte("cmd-a")},
		{Index: 2, Term: 1, Data: []byte("cmd-b")},
		{Index: 3, Term: 2, Data: []byte("cmd-c")},
	}

	// Write and close.
	s1, err := raft.NewWALLogStore(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if err := s1.Append(e); err != nil {
			t.Fatalf("Append index=%d: %v", e.Index, err)
		}
	}
	s1.Close()

	// Reopen and verify.
	s2, err := raft.NewWALLogStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	got, err := s2.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(got) != len(entries) {
		t.Fatalf("len: want %d got %d", len(entries), len(got))
	}
	for i, want := range entries {
		if got[i].Index != want.Index || got[i].Term != want.Term {
			t.Errorf("[%d]: want {%d %d} got {%d %d}", i, want.Index, want.Term, got[i].Index, got[i].Term)
		}
		if string(got[i].Data) != string(want.Data) {
			t.Errorf("[%d] data: want %q got %q", i, want.Data, got[i].Data)
		}
	}
}

// TestWALLogStore_TruncateSuffix verifies that a truncation marker is replayed
// correctly: entries at and after fromIndex are discarded.
func TestWALLogStore_TruncateSuffix(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "raft_log.wal")

	s, err := raft.NewWALLogStore(path)
	if err != nil {
		t.Fatal(err)
	}

	for _, e := range []raft.LogEntry{
		{Index: 1, Term: 1},
		{Index: 2, Term: 1},
		{Index: 3, Term: 2},
		{Index: 4, Term: 2},
	} {
		if err := s.Append(e); err != nil {
			t.Fatal(err)
		}
	}

	// Truncate from index 3 onward.
	if err := s.TruncateSuffix(3); err != nil {
		t.Fatalf("TruncateSuffix: %v", err)
	}
	s.Close()

	// Reopen and verify only indices 1,2 remain.
	s2, err := raft.NewWALLogStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	got, err := s2.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 entries, got %d: %v", len(got), got)
	}
	if got[0].Index != 1 || got[1].Index != 2 {
		t.Errorf("unexpected entries: %v", got)
	}
}

// TestWALLogStore_TruncateAndReappend verifies that after a truncation,
// new entries with the same indices (different term) can be re-appended
// and are recovered correctly.
func TestWALLogStore_TruncateAndReappend(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "raft_log.wal")

	s, err := raft.NewWALLogStore(path)
	if err != nil {
		t.Fatal(err)
	}

	// Append entries from old term.
	for _, e := range []raft.LogEntry{
		{Index: 1, Term: 1},
		{Index: 2, Term: 1},
		{Index: 3, Term: 1},
	} {
		s.Append(e)
	}

	// Conflict at index 2: truncate from 2, re-append with new term.
	s.TruncateSuffix(2)
	s.Append(raft.LogEntry{Index: 2, Term: 3, Data: []byte("new")})
	s.Append(raft.LogEntry{Index: 3, Term: 3, Data: []byte("new2")})
	s.Close()

	s2, err := raft.NewWALLogStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	got, err := s2.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 entries, got %d", len(got))
	}
	if got[0].Term != 1 || got[1].Term != 3 || got[2].Term != 3 {
		t.Errorf("unexpected terms: %v", got)
	}
	if string(got[1].Data) != "new" {
		t.Errorf("unexpected data at index 2: %q", got[1].Data)
	}
}

// TestWALLogStore_EmptyFile verifies that LoadAll on a non-existent path
// returns an empty slice without error (first startup).
func TestWALLogStore_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "raft_log.wal")

	// Open a store but write nothing.
	s, err := raft.NewWALLogStore(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	// Remove the file to simulate first startup.
	os.Remove(path)

	s2, err := raft.NewWALLogStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	got, err := s2.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll on empty: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want empty, got %v", got)
	}
}

// TestWALLogStore_NilDataEntry verifies that entries with nil Data round-trip
// correctly (heartbeat-style entries with no payload).
func TestWALLogStore_NilDataEntry(t *testing.T) {
	s, cleanup := newTestWALLogStore(t)
	defer cleanup()

	s.Append(raft.LogEntry{Index: 1, Term: 1, Data: nil})
	s.Append(raft.LogEntry{Index: 2, Term: 1, Data: []byte{}})

	got, err := s.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2, got %d", len(got))
	}
	for _, e := range got {
		if len(e.Data) != 0 {
			t.Errorf("index %d: expected empty data, got %v", e.Index, e.Data)
		}
	}
}

// TestRaftNode_LogRestoredOnStartup verifies end-to-end: a RaftNode that
// receives AppendEntries persists the entries, and a new RaftNode started
// with the same WALLogStore path recovers the log correctly.
func TestRaftNode_LogRestoredOnStartup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "raft_log.wal")

	// Node 1: receives AppendEntries with 2 entries.
	store1, err := raft.NewWALLogStore(path)
	if err != nil {
		t.Fatal(err)
	}

	node1 := raft.NewRaftNode("node-1", nil, nil, store1)
	node1.ForceRole(raft.RoleFollower, 1)

	res := node1.HandleAppendEntries(raft.AppendEntriesArgs{
		Term:         1,
		LeaderID:     "leader",
		PrevLogIndex: 0,
		PrevLogTerm:  0,
		Entries: []*pb.LogEntry{
			{Index: 1, Term: 1, Data: []byte("a")},
			{Index: 2, Term: 1, Data: []byte("b")},
		},
		LeaderCommit: 0,
	})
	if !res.Success {
		t.Fatalf("HandleAppendEntries failed: %+v", res)
	}
	if node1.LogLen() != 2 {
		t.Fatalf("node1 log: want 2, got %d", node1.LogLen())
	}
	store1.Close()

	// Node 2: restarted from the same WAL file.
	store2, err := raft.NewWALLogStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()

	node2 := raft.NewRaftNode("node-1", nil, nil, store2)
	if node2.LogLen() != 2 {
		t.Fatalf("node2 (recovered) log: want 2, got %d", node2.LogLen())
	}
}
