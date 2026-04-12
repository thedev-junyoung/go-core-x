package raft

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
)

// RaftKVCommand is the payload stored in LogEntry.Data for Raft KV operations.
type RaftKVCommand struct {
	Op    string `json:"op"`    // "set" | "del"
	Key   string `json:"key"`
	Value string `json:"value"` // empty for "del"
}

// KVStateMachine consumes committed LogEntry values from ApplyCh and maintains
// an in-memory key-value store. It also supports synchronous apply-wait via
// WaitForIndex, which lets HTTP handlers block until a specific log index has
// been applied before returning a response.
type KVStateMachine struct {
	mu   sync.RWMutex
	data map[string]string

	waitMu  sync.Mutex
	waiters map[int64][]chan struct{}
}

// NewKVStateMachine creates an empty KVStateMachine.
func NewKVStateMachine() *KVStateMachine {
	return &KVStateMachine{
		data:    make(map[string]string),
		waiters: make(map[int64][]chan struct{}),
	}
}

// Run consumes entries from applyCh until ctx is cancelled.
// Call this in a dedicated goroutine after raftNode.Run().
func (sm *KVStateMachine) Run(ctx context.Context, applyCh <-chan LogEntry) {
	for {
		select {
		case <-ctx.Done():
			return
		case entry, ok := <-applyCh:
			if !ok {
				return
			}
			sm.apply(entry)
		}
	}
}

func (sm *KVStateMachine) apply(entry LogEntry) {
	var cmd RaftKVCommand
	if err := json.Unmarshal(entry.Data, &cmd); err != nil {
		slog.Warn("raft: kv state machine ignored malformed entry",
			"index", entry.Index, "err", err)
		sm.notifyWaiters(entry.Index)
		return
	}

	sm.mu.Lock()
	switch cmd.Op {
	case "set":
		sm.data[cmd.Key] = cmd.Value
	case "del":
		delete(sm.data, cmd.Key)
	default:
		slog.Warn("raft: kv state machine unknown op", "op", cmd.Op, "index", entry.Index)
	}
	sm.mu.Unlock()

	slog.Debug("raft: kv applied", "op", cmd.Op, "key", cmd.Key, "index", entry.Index)
	sm.notifyWaiters(entry.Index)
}

// Get returns the value for key and whether it exists.
func (sm *KVStateMachine) Get(key string) (string, bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	v, ok := sm.data[key]
	return v, ok
}

// WaitForIndex blocks until the entry at index has been applied, or ctx
// expires. Used by HTTP handlers for linearizable reads-after-writes.
func (sm *KVStateMachine) WaitForIndex(ctx context.Context, index int64) error {
	ch := make(chan struct{}, 1)

	sm.waitMu.Lock()
	sm.waiters[index] = append(sm.waiters[index], ch)
	sm.waitMu.Unlock()

	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		// Clean up the registered waiter to prevent a goroutine leak.
		sm.waitMu.Lock()
		waiters := sm.waiters[index]
		filtered := waiters[:0]
		for _, w := range waiters {
			if w != ch {
				filtered = append(filtered, w)
			}
		}
		if len(filtered) == 0 {
			delete(sm.waiters, index)
		} else {
			sm.waiters[index] = filtered
		}
		sm.waitMu.Unlock()
		return ctx.Err()
	}
}

func (sm *KVStateMachine) notifyWaiters(index int64) {
	sm.waitMu.Lock()
	waiters := sm.waiters[index]
	delete(sm.waiters, index)
	sm.waitMu.Unlock()

	for _, ch := range waiters {
		close(ch)
	}
}
