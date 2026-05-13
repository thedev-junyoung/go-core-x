package raft

// Unit tests for MVCCStateMachine (ADR-022).
//
// These tests exercise the apply loop, CAS semantics, version GC,
// snapshot/restore, and WaitForIndex — all without a real RaftNode.
// Entries are applied directly via apply() to avoid election timeout delays.

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// mvccEntry builds a LogEntry with the given RaftKVCommand.
func mvccEntry(t *testing.T, index int64, cmd RaftKVCommand) LogEntry {
	t.Helper()
	data, err := json.Marshal(cmd)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return LogEntry{Index: index, Term: 1, Data: data}
}

// TestMVCC_UnconditionalWrite verifies that expected_version==0 always writes
// regardless of current state (INV-MV4).
func TestMVCC_UnconditionalWrite(t *testing.T) {
	sm := NewMVCCStateMachine(nil, 0)

	sm.apply(mvccEntry(t, 1, RaftKVCommand{Op: "set", Key: "k", Value: "v1"}))
	sm.apply(mvccEntry(t, 2, RaftKVCommand{Op: "set", Key: "k", Value: "v2"}))

	val, ver, found := sm.Get("k")
	if !found {
		t.Fatal("expected key to exist")
	}
	if val != "v2" {
		t.Fatalf("expected v2, got %q", val)
	}
	if ver != 2 {
		t.Fatalf("expected version=2, got %d", ver)
	}
}

// TestMVCC_CASSuccess verifies that a matching expected_version succeeds and
// advances the version counter.
func TestMVCC_CASSuccess(t *testing.T) {
	sm := NewMVCCStateMachine(nil, 0)

	// Unconditional write → version 1.
	sm.apply(mvccEntry(t, 1, RaftKVCommand{Op: "set", Key: "k", Value: "v1"}))

	// CAS with expected_version=1 → should succeed → version 2.
	sm.apply(mvccEntry(t, 2, RaftKVCommand{Op: "cas", Key: "k", Value: "v2", ExpectedVersion: 1}))

	if _, conflict := sm.IsCASConflict(2); conflict {
		t.Fatal("expected CAS success at index 2, got conflict")
	}

	val, ver, _ := sm.Get("k")
	if val != "v2" || ver != 2 {
		t.Fatalf("expected v2@2, got %q@%d", val, ver)
	}
}

// TestMVCC_CASConflict verifies that a stale expected_version returns a
// conflict without advancing lastApplied (INV-MV3).
func TestMVCC_CASConflict(t *testing.T) {
	sm := NewMVCCStateMachine(nil, 0)

	sm.apply(mvccEntry(t, 1, RaftKVCommand{Op: "set", Key: "k", Value: "v1"}))
	sm.apply(mvccEntry(t, 2, RaftKVCommand{Op: "set", Key: "k", Value: "v2"}))
	// version is now 2; try CAS with stale expected_version=1.
	sm.apply(mvccEntry(t, 3, RaftKVCommand{Op: "cas", Key: "k", Value: "v_stale", ExpectedVersion: 1}))

	ver, conflict := sm.IsCASConflict(3)
	if !conflict {
		t.Fatal("expected CAS conflict at index 3")
	}
	// conflict version should be 2 (latest at conflict time).
	if ver != 2 {
		t.Fatalf("expected conflict version=2, got %d", ver)
	}

	// INV-MV3: lastApplied must NOT have advanced to 3.
	if sm.LastApplied() >= 3 {
		t.Fatalf("lastApplied should not advance on CAS conflict, got %d", sm.LastApplied())
	}

	// Value must still be v2.
	val, ver, _ := sm.Get("k")
	if val != "v2" || ver != 2 {
		t.Fatalf("expected v2@2 after conflict, got %q@%d", val, ver)
	}
}

// TestMVCC_INV_MV1 verifies that latest[key] always equals versions[key][last].Version.
func TestMVCC_INV_MV1(t *testing.T) {
	sm := NewMVCCStateMachine(nil, 0)

	for i := int64(1); i <= 5; i++ {
		sm.apply(mvccEntry(t, i, RaftKVCommand{Op: "set", Key: "x", Value: "v"}))
	}

	sm.mu.RLock()
	latestVer := sm.latest["x"]
	vs := sm.versions["x"]
	lastVer := vs[len(vs)-1].Version
	sm.mu.RUnlock()

	if latestVer != lastVer {
		t.Fatalf("INV-MV1 violated: latest=%d, versions[last]=%d", latestVer, lastVer)
	}
}

// TestMVCC_GetVersion verifies snapshot reads at specific versions (INV-MV5).
func TestMVCC_GetVersion(t *testing.T) {
	sm := NewMVCCStateMachine(nil, 0)

	sm.apply(mvccEntry(t, 1, RaftKVCommand{Op: "set", Key: "k", Value: "alpha"}))
	sm.apply(mvccEntry(t, 2, RaftKVCommand{Op: "set", Key: "k", Value: "beta"}))
	sm.apply(mvccEntry(t, 3, RaftKVCommand{Op: "set", Key: "k", Value: "gamma"}))

	cases := []struct {
		ver   uint64
		want  string
		found bool
	}{
		{1, "alpha", true},
		{2, "beta", true},
		{3, "gamma", true},
		{4, "", false}, // does not exist
	}

	for _, tc := range cases {
		got, ok := sm.GetVersion("k", tc.ver)
		if ok != tc.found {
			t.Errorf("version %d: found=%v want %v", tc.ver, ok, tc.found)
			continue
		}
		if ok && got != tc.want {
			t.Errorf("version %d: got %q, want %q", tc.ver, got, tc.want)
		}
	}
}

// TestMVCC_GC verifies that old versions are trimmed when retention is set.
func TestMVCC_GC(t *testing.T) {
	const retention = 3
	sm := NewMVCCStateMachine(nil, retention)

	for i := int64(1); i <= 5; i++ {
		sm.apply(mvccEntry(t, i, RaftKVCommand{Op: "set", Key: "k", Value: "v"}))
	}

	sm.mu.RLock()
	n := len(sm.versions["k"])
	sm.mu.RUnlock()

	if n > retention {
		t.Fatalf("expected at most %d versions after GC, got %d", retention, n)
	}

	// Versions 1 and 2 should be GC'd; 3,4,5 should remain.
	if _, ok := sm.GetVersion("k", 1); ok {
		t.Fatal("version 1 should have been GC'd")
	}
	if _, ok := sm.GetVersion("k", 5); !ok {
		t.Fatal("version 5 should still be present")
	}
}

// TestMVCC_Delete verifies that a delete marks the key as not-found.
func TestMVCC_Delete(t *testing.T) {
	sm := NewMVCCStateMachine(nil, 0)

	sm.apply(mvccEntry(t, 1, RaftKVCommand{Op: "set", Key: "k", Value: "v"}))
	sm.apply(mvccEntry(t, 2, RaftKVCommand{Op: "del", Key: "k"}))

	_, _, found := sm.Get("k")
	if found {
		t.Fatal("expected key to be not-found after delete")
	}

	// The version record exists and is marked Deleted.
	sm.mu.RLock()
	vs := sm.versions["k"]
	sm.mu.RUnlock()
	if len(vs) != 2 || !vs[1].Deleted {
		t.Fatalf("expected 2 versions with last.Deleted=true, got %v", vs)
	}
}

// TestMVCC_WaitForIndex_ConflictUnblocks verifies that WaitForIndex returns
// even when the index corresponds to a CAS conflict (INV-MV3).
func TestMVCC_WaitForIndex_ConflictUnblocks(t *testing.T) {
	sm := NewMVCCStateMachine(nil, 0)

	sm.apply(mvccEntry(t, 1, RaftKVCommand{Op: "set", Key: "k", Value: "v1"}))
	// Advance to version 2.
	sm.apply(mvccEntry(t, 2, RaftKVCommand{Op: "set", Key: "k", Value: "v2"}))

	// Register waiter before applying the conflicting entry.
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		done <- sm.WaitForIndex(ctx, 3)
	}()

	// Apply conflicting CAS — should unblock the waiter.
	sm.apply(mvccEntry(t, 3, RaftKVCommand{Op: "cas", Key: "k", Value: "stale", ExpectedVersion: 1}))

	if err := <-done; err != nil {
		t.Fatalf("WaitForIndex timed out on CAS conflict: %v", err)
	}

	if _, conflict := sm.IsCASConflict(3); !conflict {
		t.Fatal("expected IsCASConflict(3) = true")
	}
}

// TestMVCC_TakeRestoreSnapshot verifies round-trip snapshot fidelity.
func TestMVCC_TakeRestoreSnapshot(t *testing.T) {
	sm := NewMVCCStateMachine(nil, 0)

	sm.apply(mvccEntry(t, 1, RaftKVCommand{Op: "set", Key: "a", Value: "1"}))
	sm.apply(mvccEntry(t, 2, RaftKVCommand{Op: "set", Key: "b", Value: "2"}))
	sm.apply(mvccEntry(t, 3, RaftKVCommand{Op: "set", Key: "a", Value: "3"}))

	snap, idx, err := sm.TakeSnapshot()
	if err != nil {
		t.Fatalf("TakeSnapshot: %v", err)
	}
	if idx != 3 {
		t.Fatalf("expected lastApplied=3, got %d", idx)
	}

	// Restore into a fresh state machine.
	sm2 := NewMVCCStateMachine(nil, 0)
	if err := sm2.RestoreSnapshot(snap, idx); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}

	// INV-MV1: latest should be restored correctly.
	val, ver, found := sm2.Get("a")
	if !found || val != "3" || ver != 2 {
		t.Fatalf("restored 'a': expected '3'@2, got %q@%d found=%v", val, ver, found)
	}

	val, ver, found = sm2.Get("b")
	if !found || val != "2" || ver != 1 {
		t.Fatalf("restored 'b': expected '2'@1, got %q@%d found=%v", val, ver, found)
	}

	if sm2.LastApplied() != 3 {
		t.Fatalf("expected LastApplied=3, got %d", sm2.LastApplied())
	}
}

// BenchmarkMVCC_UnconditionalWrite measures the hot write path throughput.
func BenchmarkMVCC_UnconditionalWrite(b *testing.B) {
	sm := NewMVCCStateMachine(nil, 10)
	data, _ := json.Marshal(RaftKVCommand{Op: "set", Key: "bench", Value: "val"})
	entry := LogEntry{Index: 0, Term: 1, Data: data}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		entry.Index = int64(i + 1)
		sm.apply(entry)
	}
}

// BenchmarkMVCC_Get measures the hot read path (latest version).
func BenchmarkMVCC_Get(b *testing.B) {
	sm := NewMVCCStateMachine(nil, 10)
	data, _ := json.Marshal(RaftKVCommand{Op: "set", Key: "bench", Value: "val"})
	sm.apply(LogEntry{Index: 1, Term: 1, Data: data})

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sm.Get("bench")
	}
}
