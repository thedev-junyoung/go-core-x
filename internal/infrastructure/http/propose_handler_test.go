package http

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	infraraft "github.com/junyoung/core-x/internal/infrastructure/raft"
)

// startLeader spins up a single-node RaftNode and waits until it is leader.
func startLeader(t *testing.T) (*infraraft.RaftNode, context.CancelFunc) {
	t.Helper()
	node := infraraft.NewRaftNode("n1", nil, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	go node.Run(ctx)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if node.Role() == infraraft.RoleLeader {
			return node, cancel
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	t.Fatal("node did not become leader in time")
	return nil, nil
}

func TestProposeHandler_SetAndRead(t *testing.T) {
	node, cancel := startLeader(t)
	defer cancel()

	sm := infraraft.NewKVStateMachine()
	smCtx, smCancel := context.WithCancel(context.Background())
	defer smCancel()
	go sm.Run(smCtx, node.ApplyCh())

	h := NewProposeHandler(node, sm)

	body := `{"key":"name","value":"core-x"}`
	req := httptest.NewRequest(http.MethodPost, "/raft/kv", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d body=%s", rec.Code, rec.Body.String())
	}

	v, ok := sm.Get("name")
	if !ok || v != "core-x" {
		t.Fatalf("expected name=core-x, got %q ok=%v", v, ok)
	}
}

func TestProposeHandler_NotLeader(t *testing.T) {
	// A node that has never run will not be leader.
	node := infraraft.NewRaftNode("n1", nil, nil, nil)
	sm := infraraft.NewKVStateMachine()
	h := NewProposeHandler(node, sm)

	body := `{"key":"k","value":"v"}`
	req := httptest.NewRequest(http.MethodPost, "/raft/kv", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
}

func TestProposeHandler_MissingKey(t *testing.T) {
	node := infraraft.NewRaftNode("n1", nil, nil, nil)
	sm := infraraft.NewKVStateMachine()
	h := NewProposeHandler(node, sm)

	req := httptest.NewRequest(http.MethodPost, "/raft/kv", bytes.NewBufferString(`{"value":"v"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestRaftKVGetHandler_FoundAndNotFound(t *testing.T) {
	sm := infraraft.NewKVStateMachine()

	// Manually seed the state machine by sending entries directly.
	ch := make(chan infraraft.LogEntry, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sm.Run(ctx, ch)

	data, _ := json.Marshal(infraraft.RaftKVCommand{Op: "set", Key: "lang", Value: "go"})
	ch <- infraraft.LogEntry{Index: 1, Term: 1, Data: data}

	waitCtx, waitCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer waitCancel()
	if err := sm.WaitForIndex(waitCtx, 1); err != nil {
		t.Fatalf("WaitForIndex: %v", err)
	}

	getH := NewRaftKVGetHandler(sm)

	// Found.
	req := httptest.NewRequest(http.MethodGet, "/raft/kv/lang", nil)
	req.SetPathValue("key", "lang")
	rec := httptest.NewRecorder()
	getH.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp raftKVGetResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Value != "go" {
		t.Fatalf("expected value=go, got %q", resp.Value)
	}

	// Not found.
	req2 := httptest.NewRequest(http.MethodGet, "/raft/kv/missing", nil)
	req2.SetPathValue("key", "missing")
	rec2 := httptest.NewRecorder()
	getH.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec2.Code)
	}
}
