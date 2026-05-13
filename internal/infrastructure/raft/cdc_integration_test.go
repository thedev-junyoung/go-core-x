package raft_test

// cdc_integration_test.go — Integration tests: MVCCStateMachine + ChangeLog (ADR-023).
//
// Scenarios:
//   1. 10 set/del writes → subscriber receives 10 events in order (INV-CDC1, INV-CDC2)
//   2. CAS conflict → NOT published (INV-CDC1)
//   3. Offset replay: subscriber starts at offset=5 → receives events 5..10 (INV-CDC6)
//   4. CDC disabled (nil changeLog) → no panic, apply still works

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	. "github.com/junyoung/core-x/internal/infrastructure/raft"
)

// applyEntry applies a RaftKVCommand to sm via the applyCh goroutine model.
// Returns the log index used.
func applyEntry(t *testing.T, applyCh chan<- LogEntry, index int64, cmd RaftKVCommand) {
	t.Helper()
	data, err := json.Marshal(cmd)
	if err != nil {
		t.Fatalf("marshal cmd: %v", err)
	}
	applyCh <- LogEntry{Index: index, Data: data}
}

// waitApplied blocks until sm.LastApplied() >= index or the deadline passes.
func waitApplied(t *testing.T, sm *MVCCStateMachine, index int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := sm.WaitForIndex(ctx, index); err != nil {
		t.Fatalf("WaitForIndex(%d): %v", index, err)
	}
}

// startMVCCSM creates a MVCCStateMachine with an associated ChangeLog,
// starts its Run goroutine, and returns the cancel func.
func startMVCCSM(t *testing.T, cl *ChangeLog) (*MVCCStateMachine, chan<- LogEntry, context.CancelFunc) {
	t.Helper()
	sm := NewMVCCStateMachine(nil, 0)
	sm.SetChangeLog(cl)

	applyCh := make(chan LogEntry, 64)
	ctx, cancel := context.WithCancel(context.Background())
	go sm.Run(ctx, applyCh)
	return sm, applyCh, cancel
}

// TestCDCIntegration_OrderedEvents verifies that 10 set/del writes produce
// 10 ordered CDC events received by a subscriber. (INV-CDC1, INV-CDC2)
func TestCDCIntegration_OrderedEvents(t *testing.T) {
	cl := NewChangeLog(100, nil)
	defer cl.Close()

	sm, applyCh, cancel := startMVCCSM(t, cl)
	defer cancel()

	id, ch, err := cl.Subscribe(64)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cl.Unsubscribe(id)

	ops := []struct {
		op    string
		key   string
		value string
	}{
		{"set", "k1", "v1"},
		{"set", "k2", "v2"},
		{"set", "k1", "v1b"},
		{"del", "k2", ""},
		{"set", "k3", "v3"},
		{"set", "k4", "v4"},
		{"del", "k3", ""},
		{"set", "k5", "v5"},
		{"set", "k1", "v1c"},
		{"del", "k5", ""},
	}

	for i, op := range ops {
		applyEntry(t, applyCh, int64(i+1), RaftKVCommand{
			Op:    op.op,
			Key:   op.key,
			Value: op.value,
		})
	}

	// Wait for last entry to be applied before collecting events.
	waitApplied(t, sm, int64(len(ops)))

	// Collect all events with a generous deadline.
	received := make([]ChangeEvent, 0, len(ops))
	deadline := time.After(2 * time.Second)
collect:
	for {
		select {
		case ev := <-ch:
			received = append(received, ev)
			if len(received) == len(ops) {
				break collect
			}
		case <-deadline:
			break collect
		}
	}

	if len(received) != len(ops) {
		t.Fatalf("expected %d events, received %d", len(ops), len(received))
	}

	// Verify ordering: offsets must be strictly increasing. (INV-CDC2)
	for i, ev := range received {
		wantOffset := int64(i + 1)
		if ev.Offset != wantOffset {
			t.Errorf("event[%d].Offset = %d, want %d", i, ev.Offset, wantOffset)
		}
	}

	// Verify event types match the ops.
	for i, ev := range received {
		if ev.Type != ops[i].op {
			t.Errorf("event[%d].Type = %q, want %q", i, ev.Type, ops[i].op)
		}
		if ev.Key != ops[i].key {
			t.Errorf("event[%d].Key = %q, want %q", i, ev.Key, ops[i].key)
		}
	}
}

// TestCDCIntegration_CASConflictNotPublished verifies that a CAS conflict does
// NOT produce a CDC event (INV-CDC1, INV-MV3).
func TestCDCIntegration_CASConflictNotPublished(t *testing.T) {
	cl := NewChangeLog(100, nil)
	defer cl.Close()

	sm, applyCh, cancel := startMVCCSM(t, cl)
	defer cancel()

	id, ch, err := cl.Subscribe(32)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cl.Unsubscribe(id)

	// Index 1: unconditional set (creates version=1 for "stock").
	applyEntry(t, applyCh, 1, RaftKVCommand{Op: "set", Key: "stock", Value: "100"})
	waitApplied(t, sm, 1)

	// Index 2: CAS with wrong expected_version (2, but current is 1) → conflict.
	applyEntry(t, applyCh, 2, RaftKVCommand{
		Op: "set", Key: "stock", Value: "90",
		ExpectedVersion: 2, // wrong: current is 1
	})
	// Wait until the conflict is recorded (WaitForIndex returns nil on conflict).
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	if err := sm.WaitForIndex(ctx2, 2); err != nil {
		t.Fatalf("WaitForIndex(2) conflict: %v", err)
	}

	// Index 3: successful unconditional write.
	applyEntry(t, applyCh, 3, RaftKVCommand{Op: "set", Key: "stock", Value: "80"})
	waitApplied(t, sm, 3)

	// Collect up to 200ms.
	var events []ChangeEvent
	deadline := time.After(200 * time.Millisecond)
collect:
	for {
		select {
		case ev := <-ch:
			events = append(events, ev)
		case <-deadline:
			break collect
		}
	}

	// Expect exactly 2 events: offset=1 (set) and offset=3 (set).
	// The conflict at offset=2 must NOT appear. (INV-CDC1)
	if len(events) != 2 {
		t.Fatalf("expected 2 events (offsets 1, 3), got %d: %+v", len(events), events)
	}
	if events[0].Offset != 1 {
		t.Errorf("events[0].Offset = %d, want 1", events[0].Offset)
	}
	if events[1].Offset != 3 {
		t.Errorf("events[1].Offset = %d, want 3", events[1].Offset)
	}
}

// TestCDCIntegration_OffsetReplay verifies that a subscriber requesting
// replay from offset=5 receives events 5..10, then live events continue.
func TestCDCIntegration_OffsetReplay(t *testing.T) {
	cl := NewChangeLog(100, nil)
	defer cl.Close()

	sm, applyCh, cancel := startMVCCSM(t, cl)
	defer cancel()

	// Publish 10 events (offsets 1..10).
	for i := range int64(10) {
		applyEntry(t, applyCh, i+1, RaftKVCommand{Op: "set", Key: "k", Value: "v"})
	}
	waitApplied(t, sm, 10)

	// Replay from offset 5 (history-based; no live subscription yet).
	replayed, err := cl.ReplayFrom(5)
	if err != nil {
		t.Fatalf("ReplayFrom(5): %v", err)
	}
	if len(replayed) != 6 {
		t.Fatalf("expected 6 replayed events (5..10), got %d", len(replayed))
	}
	for i, ev := range replayed {
		want := int64(5 + i)
		if ev.Offset != want {
			t.Errorf("replayed[%d].Offset = %d, want %d", i, ev.Offset, want)
		}
	}

	// Now subscribe and receive a live event (offset=11).
	id, ch, _ := cl.Subscribe(16)
	defer cl.Unsubscribe(id)

	applyEntry(t, applyCh, 11, RaftKVCommand{Op: "set", Key: "k", Value: "live"})
	waitApplied(t, sm, 11)

	select {
	case ev := <-ch:
		if ev.Offset != 11 {
			t.Errorf("live event.Offset = %d, want 11", ev.Offset)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout waiting for live event after replay")
	}
}

// TestCDCIntegration_NilChangeLog verifies that MVCCStateMachine works normally
// when no ChangeLog is configured (CDC disabled — backward-compatible).
func TestCDCIntegration_NilChangeLog(t *testing.T) {
	sm := NewMVCCStateMachine(nil, 0)
	// SetChangeLog(nil) is valid — CDC disabled.
	sm.SetChangeLog(nil)

	applyCh := make(chan LogEntry, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sm.Run(ctx, applyCh)

	applyEntry(t, applyCh, 1, RaftKVCommand{Op: "set", Key: "x", Value: "1"})
	waitApplied(t, sm, 1)

	val, ver, found := sm.Get("x")
	if !found || val != "1" || ver != 1 {
		t.Errorf("Get: found=%v val=%q ver=%d, want found=true val=\"1\" ver=1", found, val, ver)
	}
}

// TestCDCIntegration_VersionInEvent verifies that ChangeEvent.Version matches
// the MVCC version assigned by the state machine (INV-CDC3).
func TestCDCIntegration_VersionInEvent(t *testing.T) {
	cl := NewChangeLog(100, nil)
	defer cl.Close()

	sm, applyCh, cancel := startMVCCSM(t, cl)
	defer cancel()

	id, ch, _ := cl.Subscribe(16)
	defer cl.Unsubscribe(id)

	// Three successive writes to the same key; versions should be 1, 2, 3.
	for i := range int64(3) {
		applyEntry(t, applyCh, i+1, RaftKVCommand{Op: "set", Key: "vkey", Value: "v"})
	}
	waitApplied(t, sm, 3)

	for wantVer := uint64(1); wantVer <= 3; wantVer++ {
		select {
		case ev := <-ch:
			if ev.Version != wantVer {
				t.Errorf("event version %d, want %d", ev.Version, wantVer)
			}
			// INV-CDC3: event version must match what sm reports.
			_, smVer, _ := sm.Get("vkey")
			// smVer reflects the latest version; after all 3 applies it is 3.
			// Check that the published version is monotonically increasing.
			if ev.Version < 1 || ev.Version > smVer {
				t.Errorf("event.Version=%d out of range [1, %d]", ev.Version, smVer)
			}
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("timeout waiting for version %d event", wantVer)
		}
	}
}
