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

// startLeaderMVCC spins up a single-node Raft leader + MVCCStateMachine pair.
// Returns the node, sm, a registry pre-populated with the node, and a cancel func.
func startLeaderMVCC(t *testing.T) (*infraraft.RaftNode, *infraraft.MVCCStateMachine, *infraraft.RaftGroupRegistry, context.CancelFunc) {
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

	sm := infraraft.NewMVCCStateMachine(nil, 0) // retention=0 keeps all versions
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

// casBody serialises a CAS write request body.
func casBody(t *testing.T, value string, expectedVersion uint64) *bytes.Buffer {
	t.Helper()
	b, err := json.Marshal(map[string]any{"value": value, "expected_version": expectedVersion})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return bytes.NewBuffer(b)
}

// doCASWrite issues PUT /mvcc/kv/{key} and returns the recorder.
func doCASWrite(t *testing.T, h http.Handler, key, value string, expectedVersion uint64) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/mvcc/kv/"+key, casBody(t, value, expectedVersion))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("key", key)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestCASHandler_Unconditional verifies that expected_version=0 always succeeds (INV-MV4).
func TestCASHandler_Unconditional(t *testing.T) {
	_, _, registry, cancel := startLeaderMVCC(t)
	defer cancel()

	sm := infraraft.NewMVCCStateMachine(nil, 0)
	// Use a fresh sm wired to the same node via re-register.
	node := infraraft.NewRaftNode("n1", nil, nil, nil)
	nodeCtx, nodeCancel := context.WithCancel(context.Background())
	defer nodeCancel()
	go node.Run(nodeCtx)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if node.Role() == infraraft.RoleLeader {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if node.Role() != infraraft.RoleLeader {
		t.Fatal("node did not become leader in time")
	}

	smCtx, smCancel := context.WithCancel(context.Background())
	defer smCancel()
	go sm.Run(smCtx, node.ApplyCh())

	reg2 := infraraft.NewRaftGroupRegistry()
	reg2.Register(infraraft.PartitionID("n1"), node)

	_ = registry // suppress unused warning from outer startLeaderMVCC

	h := infrahttp.NewCASHandler(reg2, sm, "n1", nil)

	rec := doCASWrite(t, h, "counter", "100", 0)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("unconditional write: expected 204, got %d: %s", rec.Code, rec.Body.String())
	}

	// Second unconditional write must also succeed regardless of current version.
	rec2 := doCASWrite(t, h, "counter", "200", 0)
	if rec2.Code != http.StatusNoContent {
		t.Fatalf("second unconditional write: expected 204, got %d: %s", rec2.Code, rec2.Body.String())
	}
}

// TestCASHandler_Conflict verifies that a stale expected_version returns 409 (INV-MV3).
//
// Scenario:
//  1. Unconditional write → version becomes 1.
//  2. CAS with expected_version=1 → succeeds → version becomes 2.
//  3. CAS with expected_version=1 again → stale → 409 Conflict.
func TestCASHandler_Conflict(t *testing.T) {
	node, sm, registry, cancel := startLeaderMVCC(t)
	defer cancel()
	_ = node

	h := infrahttp.NewCASHandler(registry, sm, "n1", nil)

	// Step 1: write initial value unconditionally.
	rec := doCASWrite(t, h, "stock", "100", 0)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("step 1: expected 204, got %d: %s", rec.Code, rec.Body.String())
	}

	// Step 2: CAS with expected_version=1 (should succeed → version 2).
	rec = doCASWrite(t, h, "stock", "90", 1)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("step 2: expected 204, got %d: %s", rec.Code, rec.Body.String())
	}

	// Step 3: CAS with expected_version=1 again — now stale → 409.
	rec = doCASWrite(t, h, "stock", "80", 1)
	if rec.Code != http.StatusConflict {
		t.Fatalf("step 3: expected 409, got %d: %s", rec.Code, rec.Body.String())
	}

	var conflict struct {
		CurrentVersion uint64 `json:"current_version"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&conflict); err != nil {
		t.Fatalf("decode conflict response: %v", err)
	}
	if conflict.CurrentVersion != 2 {
		t.Fatalf("expected current_version=2, got %d", conflict.CurrentVersion)
	}
}

// TestSnapshotRead verifies that GET /mvcc/kv/{key}?version=N returns the value
// at that specific version without going through ReadIndex (INV-MV5).
func TestSnapshotRead(t *testing.T) {
	node, sm, registry, cancel := startLeaderMVCC(t)
	defer cancel()
	_ = node

	casH := infrahttp.NewCASHandler(registry, sm, "n1", nil)
	getH := infrahttp.NewMVCCKVHandler(registry, sm, "n1", nil)

	// Write version 1: "hello".
	rec := doCASWrite(t, casH, "greeting", "hello", 0)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("write v1: expected 204, got %d: %s", rec.Code, rec.Body.String())
	}

	// Write version 2: "world".
	rec = doCASWrite(t, casH, "greeting", "world", 1)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("write v2: expected 204, got %d: %s", rec.Code, rec.Body.String())
	}

	// Snapshot read at version=1 must return "hello" (INV-MV5).
	req := httptest.NewRequest(http.MethodGet, "/mvcc/kv/greeting?version=1", nil)
	req.SetPathValue("key", "greeting")
	recGet := httptest.NewRecorder()
	getH.ServeHTTP(recGet, req)

	if recGet.Code != http.StatusOK {
		t.Fatalf("snapshot read v1: expected 200, got %d: %s", recGet.Code, recGet.Body.String())
	}

	var resp struct {
		Key     string `json:"key"`
		Value   string `json:"value"`
		Version uint64 `json:"version"`
	}
	if err := json.NewDecoder(recGet.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Value != "hello" {
		t.Fatalf("expected value=hello at version 1, got %q", resp.Value)
	}
	if resp.Version != 1 {
		t.Fatalf("expected version=1, got %d", resp.Version)
	}

	// Snapshot read at version=2 must return "world".
	req2 := httptest.NewRequest(http.MethodGet, "/mvcc/kv/greeting?version=2", nil)
	req2.SetPathValue("key", "greeting")
	recGet2 := httptest.NewRecorder()
	getH.ServeHTTP(recGet2, req2)

	if recGet2.Code != http.StatusOK {
		t.Fatalf("snapshot read v2: expected 200, got %d: %s", recGet2.Code, recGet2.Body.String())
	}
	var resp2 struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(recGet2.Body).Decode(&resp2); err != nil {
		t.Fatalf("decode v2: %v", err)
	}
	if resp2.Value != "world" {
		t.Fatalf("expected value=world at version 2, got %q", resp2.Value)
	}
}

// TestCASHandler_NotLeader verifies that a non-leader node returns 503.
func TestCASHandler_NotLeader(t *testing.T) {
	node := infraraft.NewRaftNode("n1", nil, nil, nil) // never started → not leader
	sm := infraraft.NewMVCCStateMachine(nil, 0)
	registry := infraraft.NewRaftGroupRegistry()
	registry.Register(infraraft.PartitionID("n1"), node)

	h := infrahttp.NewCASHandler(registry, sm, "n1", nil)

	rec := doCASWrite(t, h, "key", "val", 0)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestMVCCKVHandler_SnapshotRead_InvalidVersion verifies that an invalid
// ?version parameter returns 400.
func TestMVCCKVHandler_SnapshotRead_InvalidVersion(t *testing.T) {
	_, sm, registry, cancel := startLeaderMVCC(t)
	defer cancel()

	h := infrahttp.NewMVCCKVHandler(registry, sm, "n1", nil)

	req := httptest.NewRequest(http.MethodGet, "/mvcc/kv/key?version=abc", nil)
	req.SetPathValue("key", "key")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestMVCCKVHandler_SnapshotRead_NotFound verifies that requesting a GC'd
// or non-existent version returns 404.
func TestMVCCKVHandler_SnapshotRead_NotFound(t *testing.T) {
	_, sm, registry, cancel := startLeaderMVCC(t)
	defer cancel()

	h := infrahttp.NewMVCCKVHandler(registry, sm, "n1", nil)

	req := httptest.NewRequest(http.MethodGet, "/mvcc/kv/nokey?version=999", nil)
	req.SetPathValue("key", "nokey")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}
