package raft

import (
	"encoding/json"
	"testing"
)

// BenchmarkKV_UnconditionalWrite is the baseline for measuring MVCC version-
// tracking overhead. It mirrors BenchmarkMVCC_UnconditionalWrite using the
// non-versioned KVStateMachine.
func BenchmarkKV_UnconditionalWrite(b *testing.B) {
	sm := NewKVStateMachine(nil)
	data, _ := json.Marshal(RaftKVCommand{Op: "set", Key: "bench", Value: "val"})
	entry := LogEntry{Index: 0, Term: 1, Data: data}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		entry.Index = int64(i + 1)
		sm.apply(entry)
	}
}

// BenchmarkKV_Get is the baseline for measuring MVCC read-path overhead.
// It mirrors BenchmarkMVCC_Get using the non-versioned KVStateMachine.
func BenchmarkKV_Get(b *testing.B) {
	sm := NewKVStateMachine(nil)
	data, _ := json.Marshal(RaftKVCommand{Op: "set", Key: "bench", Value: "val"})
	sm.apply(LogEntry{Index: 1, Term: 1, Data: data})

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sm.Get("bench")
	}
}

// BenchmarkMVCC_CASSuccess measures the CAS success path: every write provides
// the correct expected_version, so the version-check passes and a new version
// is appended. This captures the extra work over an unconditional write
// (the version equality check + the same append).
func BenchmarkMVCC_CASSuccess(b *testing.B) {
	sm := NewMVCCStateMachine(nil, 0) // retention=0 → no GC, isolate CAS cost
	initData, _ := json.Marshal(RaftKVCommand{Op: "set", Key: "bench", Value: "v0"})
	sm.apply(LogEntry{Index: 1, Term: 1, Data: initData})

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		expected := uint64(i + 1) // version after the i-th apply is i+1
		cmd := RaftKVCommand{
			Op:              "set",
			Key:             "bench",
			Value:           "val",
			ExpectedVersion: expected,
		}
		data, _ := json.Marshal(cmd)
		sm.apply(LogEntry{Index: int64(i + 2), Term: 1, Data: data})
	}
}

// BenchmarkMVCC_CASConflict measures the CAS conflict path: every attempt uses
// a stale expected_version, so the apply records a conflict and exits early
// without appending a version. This isolates the cost of the failure path.
func BenchmarkMVCC_CASConflict(b *testing.B) {
	sm := NewMVCCStateMachine(nil, 0)
	initData, _ := json.Marshal(RaftKVCommand{Op: "set", Key: "bench", Value: "v0"})
	sm.apply(LogEntry{Index: 1, Term: 1, Data: initData})

	cmd := RaftKVCommand{
		Op:              "set",
		Key:             "bench",
		Value:           "val",
		ExpectedVersion: 99999, // always stale → always conflict
	}
	data, _ := json.Marshal(cmd)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sm.apply(LogEntry{Index: int64(i + 2), Term: 1, Data: data})
	}
}
