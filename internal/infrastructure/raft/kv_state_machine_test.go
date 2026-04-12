package raft

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// makeEntry returns a LogEntry with a JSON-encoded RaftKVCommand payload.
func makeEntry(index int64, op, key, value string) LogEntry {
	data, _ := json.Marshal(RaftKVCommand{Op: op, Key: key, Value: value})
	return LogEntry{Index: index, Term: 1, Data: data}
}

func TestKVStateMachine_SetAndGet(t *testing.T) {
	sm := NewKVStateMachine()
	ch := make(chan LogEntry, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sm.Run(ctx, ch)

	ch <- makeEntry(1, "set", "foo", "bar")

	waitCtx, waitCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer waitCancel()
	if err := sm.WaitForIndex(waitCtx, 1); err != nil {
		t.Fatalf("WaitForIndex: %v", err)
	}

	v, ok := sm.Get("foo")
	if !ok || v != "bar" {
		t.Fatalf("expected foo=bar, got %q ok=%v", v, ok)
	}
}

func TestKVStateMachine_Del(t *testing.T) {
	sm := NewKVStateMachine()
	ch := make(chan LogEntry, 2)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sm.Run(ctx, ch)

	ch <- makeEntry(1, "set", "foo", "bar")
	ch <- makeEntry(2, "del", "foo", "")

	waitCtx, waitCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer waitCancel()
	if err := sm.WaitForIndex(waitCtx, 2); err != nil {
		t.Fatalf("WaitForIndex: %v", err)
	}

	_, ok := sm.Get("foo")
	if ok {
		t.Fatal("expected foo to be deleted")
	}
}

func TestKVStateMachine_WaitForIndex_Timeout(t *testing.T) {
	sm := NewKVStateMachine()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	// No entry is ever applied — should time out.
	err := sm.WaitForIndex(ctx, 99)
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestKVStateMachine_MalformedEntry(t *testing.T) {
	sm := NewKVStateMachine()
	ch := make(chan LogEntry, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sm.Run(ctx, ch)

	// Malformed JSON — should be ignored gracefully; waiters still notified.
	ch <- LogEntry{Index: 1, Term: 1, Data: []byte("not json")}

	waitCtx, waitCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer waitCancel()
	if err := sm.WaitForIndex(waitCtx, 1); err != nil {
		t.Fatalf("WaitForIndex on malformed entry: %v", err)
	}
}

// TestSingleNode_ProposeApplyRoundTrip verifies the full Propose → ApplyCh →
// KVStateMachine round-trip on a single-node cluster.
func TestSingleNode_ProposeApplyRoundTrip(t *testing.T) {
	node := NewRaftNode("n1", nil, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go node.Run(ctx)

	sm := NewKVStateMachine()
	go sm.Run(ctx, node.ApplyCh())

	// Wait until the node becomes leader.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if node.Role() == RoleLeader {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if node.Role() != RoleLeader {
		t.Fatal("node did not become leader")
	}

	// Propose a set command.
	cmd := RaftKVCommand{Op: "set", Key: "hello", Value: "world"}
	data, _ := json.Marshal(cmd)
	index, _, isLeader := node.Propose(data)
	if !isLeader {
		t.Fatal("expected isLeader=true")
	}

	// Wait for the state machine to apply the entry.
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer waitCancel()
	if err := sm.WaitForIndex(waitCtx, index); err != nil {
		t.Fatalf("WaitForIndex: %v", err)
	}

	v, ok := sm.Get("hello")
	if !ok || v != "world" {
		t.Fatalf("expected hello=world, got %q ok=%v", v, ok)
	}
}
