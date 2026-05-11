package http_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	infrahttp "github.com/junyoung/core-x/internal/infrastructure/http"
	infraraft "github.com/junyoung/core-x/internal/infrastructure/raft"
)

// testMarshal serialises v as a *bytes.Buffer for use in httptest requests.
func testMarshal(t *testing.T, v any) *bytes.Buffer {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return bytes.NewBuffer(b)
}

// startLeaderSM spins up a single-node Raft leader + KVStateMachine pair.
// Returns the node, sm, a registry pre-populated with the node, and a cancel func.
func startLeaderSM(t *testing.T) (*infraraft.RaftNode, *infraraft.KVStateMachine, *infraraft.RaftGroupRegistry, context.CancelFunc) {
	t.Helper()
	node := infraraft.NewRaftNode("n1", nil, nil, nil)
	nodeCtx, nodeCancel := context.WithCancel(context.Background())
	go node.Run(nodeCtx)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if node.Role() == infraraft.RoleLeader {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if node.Role() != infraraft.RoleLeader {
		nodeCancel()
		t.Fatal("node did not become leader in time")
	}

	sm := infraraft.NewKVStateMachine(nil)
	smCtx, smCancel := context.WithCancel(context.Background())
	go sm.Run(smCtx, node.ApplyCh())

	registry := infraraft.NewRaftGroupRegistry()
	registry.Register(infraraft.PartitionID("n1"), node)

	cancel := func() {
		nodeCancel()
		smCancel()
	}
	return node, sm, registry, cancel
}

// TestUnifiedKVHandler_EmptyKey checks that a missing key path value returns 400.
func TestUnifiedKVHandler_EmptyKey(t *testing.T) {
	_, sm, registry, cancel := startLeaderSM(t)
	defer cancel()

	h := infrahttp.NewUnifiedKVHandler(registry, sm, "n1", nil, nil, 0, nil)

	req := httptest.NewRequest(http.MethodGet, "/kv/", nil)
	req.SetPathValue("key", "")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

// TestUnifiedKVHandler_NoPartition checks that 503 is returned when no RaftNode
// is registered for the local partitionID.
func TestUnifiedKVHandler_NoPartition(t *testing.T) {
	registry := infraraft.NewRaftGroupRegistry()
	sm := infraraft.NewKVStateMachine(nil)
	h := infrahttp.NewUnifiedKVHandler(registry, sm, "n1", nil, nil, 0, nil)

	req := httptest.NewRequest(http.MethodGet, "/kv/somekey", nil)
	req.SetPathValue("key", "somekey")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestUnifiedKVHandler_NotFound checks that a missing key returns 404.
func TestUnifiedKVHandler_NotFound(t *testing.T) {
	_, sm, registry, cancel := startLeaderSM(t)
	defer cancel()

	h := infrahttp.NewUnifiedKVHandler(registry, sm, "n1", nil, nil, 0, nil)

	req := httptest.NewRequest(http.MethodGet, "/kv/missing", nil)
	req.SetPathValue("key", "missing")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestUnifiedKVHandler_EndToEnd seeds data via ProposeHandler and reads it back
// via UnifiedKVHandler (ReadIndex → WaitForIndex → sm.Get).
func TestUnifiedKVHandler_EndToEnd(t *testing.T) {
	node, sm, registry, cancel := startLeaderSM(t)
	defer cancel()

	// Write "lang=go" through the Raft propose path.
	proposeH := infrahttp.NewProposeHandler(node, sm, nil)
	proposeReq := httptest.NewRequest(http.MethodPost, "/raft/kv",
		testMarshal(t, map[string]string{"key": "lang", "value": "go"}))
	proposeReq.Header.Set("Content-Type", "application/json")
	proposeRec := httptest.NewRecorder()
	proposeH.ServeHTTP(proposeRec, proposeReq)
	if proposeRec.Code != http.StatusNoContent {
		t.Fatalf("propose: expected 204, got %d: %s", proposeRec.Code, proposeRec.Body.String())
	}

	// Read back via UnifiedKVHandler.
	kvH := infrahttp.NewUnifiedKVHandler(registry, sm, "n1", nil, nil, 0, nil)

	req := httptest.NewRequest(http.MethodGet, "/kv/lang", nil)
	req.SetPathValue("key", "lang")
	rec := httptest.NewRecorder()
	kvH.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["value"] != "go" {
		t.Fatalf("expected value=go, got %q", resp["value"])
	}
}

// TestUnifiedHTTPHandler_UnifiedIngest_NotLeader checks that serveUnifiedIngest
// returns 503 when the Raft node is not the leader and addrMap is nil.
func TestUnifiedHTTPHandler_UnifiedIngest_NotLeader(t *testing.T) {
	// A node that has never run will not be leader.
	node := infraraft.NewRaftNode("n1", nil, nil, nil)
	sm := infraraft.NewKVStateMachine(nil)
	registry := infraraft.NewRaftGroupRegistry()
	registry.Register(infraraft.PartitionID("n1"), node)

	h := infrahttp.NewHTTPHandler(nil, "n1", nil, 0, registry, sm, nil)

	body := testMarshal(t, map[string]string{"source": "src1", "payload": "data"})
	req := httptest.NewRequest(http.MethodPost, "/ingest", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestServeUnifiedIngest_NotLeader_Redirects307 checks that serveUnifiedIngest
// returns 307 redirect when addrMap contains the current leader's HTTP base URL.
func TestServeUnifiedIngest_NotLeader_Redirects307(t *testing.T) {
	// n1 is a follower (never runs); n2 is injected as leader via ForceLeaderID.
	node := infraraft.NewRaftNode("n1", nil, nil, nil)
	node.ForceLeaderID("n2")

	sm := infraraft.NewKVStateMachine(nil)
	registry := infraraft.NewRaftGroupRegistry()
	registry.Register(infraraft.PartitionID("n1"), node)

	addrMap := map[string]string{
		"n2": "http://node2:8080",
	}
	h := infrahttp.NewHTTPHandler(nil, "n1", nil, 0, registry, sm, addrMap)

	body := testMarshal(t, map[string]string{"source": "src1", "payload": "data"})
	req := httptest.NewRequest(http.MethodPost, "/ingest", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("expected 307, got %d: %s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if loc != "http://node2:8080/ingest" {
		t.Fatalf("unexpected redirect Location: %q", loc)
	}
}

// TestServeUnifiedIngest_NotLeader_503NoHint checks that serveUnifiedIngest
// falls back to 503 when addrMap is nil (no leader hint available).
func TestServeUnifiedIngest_NotLeader_503NoHint(t *testing.T) {
	node := infraraft.NewRaftNode("n1", nil, nil, nil)
	// leaderID unknown — ForceLeaderID not called, so LeaderID() returns "".
	sm := infraraft.NewKVStateMachine(nil)
	registry := infraraft.NewRaftGroupRegistry()
	registry.Register(infraraft.PartitionID("n1"), node)

	h := infrahttp.NewHTTPHandler(nil, "n1", nil, 0, registry, sm, nil)

	body := testMarshal(t, map[string]string{"source": "src1", "payload": "data"})
	req := httptest.NewRequest(http.MethodPost, "/ingest", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestUnifiedHTTPHandler_UnifiedIngest_EndToEnd proposes via POST /ingest
// (unified path) and verifies the entry is visible in the state machine.
func TestUnifiedHTTPHandler_UnifiedIngest_EndToEnd(t *testing.T) {
	_, sm, registry, cancel := startLeaderSM(t)
	defer cancel()

	h := infrahttp.NewHTTPHandler(nil, "n1", nil, 0, registry, sm, nil)

	body := testMarshal(t, map[string]string{"source": "device-42", "payload": "hello"})
	req := httptest.NewRequest(http.MethodPost, "/ingest", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}

	value, ok := sm.Get("device-42")
	if !ok || value != "hello" {
		t.Fatalf("expected sm[device-42]=hello, got %q ok=%v", value, ok)
	}
}
