package http

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	infraraft "github.com/junyoung/core-x/internal/infrastructure/raft"
)

const proposeApplyTimeout = 5 * time.Second

// ProposeHandler handles the Raft KV write and read paths:
//
//	POST /raft/kv  — propose a set/del command to the Raft cluster
//	GET  /raft/kv/{key} — read from the local Raft KV state machine
//
// Both endpoints are served by ProposeHandler and registered separately in
// the HTTP mux to keep routing explicit.
type ProposeHandler struct {
	node    *infraraft.RaftNode
	sm      *infraraft.KVStateMachine
	addrMap map[string]string // nodeID → HTTP base URL (e.g. "http://host:port"), may be nil
}

// NewProposeHandler creates a ProposeHandler.
// addrMap maps Raft node IDs to HTTP base URLs used for leader redirect.
// Pass nil to disable redirect (returns 503 when not leader instead).
func NewProposeHandler(node *infraraft.RaftNode, sm *infraraft.KVStateMachine, addrMap map[string]string) *ProposeHandler {
	return &ProposeHandler{node: node, sm: sm, addrMap: addrMap}
}

// proposeRequest is the JSON body for POST /raft/kv.
type proposeRequest struct {
	Op    string `json:"op"`    // "set" (default) or "del"
	Key   string `json:"key"`
	Value string `json:"value"` // empty for "del"
}

// ServeHTTP handles POST /raft/kv.
//
// Flow:
//  1. Decode JSON body → RaftKVCommand
//  2. raftNode.Propose(data) → (index, term, isLeader)
//  3. If !isLeader → 503 Service Unavailable
//  4. Wait for index via KVStateMachine.WaitForIndex (5 s timeout)
//  5. 204 No Content on success
func (h *ProposeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req proposeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.Key == "" {
		http.Error(w, `"key" is required`, http.StatusBadRequest)
		return
	}
	if req.Op == "" {
		req.Op = "set"
	}
	if req.Op != "set" && req.Op != "del" {
		http.Error(w, `"op" must be "set" or "del"`, http.StatusBadRequest)
		return
	}

	cmd := infraraft.RaftKVCommand{Op: req.Op, Key: req.Key, Value: req.Value}
	data, _ := json.Marshal(cmd)

	index, _, isLeader := h.node.Propose(data)
	if !isLeader {
		if leaderID := h.node.LeaderID(); leaderID != "" {
			if baseURL, ok := h.addrMap[leaderID]; ok {
				http.Redirect(w, r, baseURL+"/raft/kv", http.StatusTemporaryRedirect)
				return
			}
		}
		http.Error(w, "not the Raft leader — retry on the leader node", http.StatusServiceUnavailable)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), proposeApplyTimeout)
	defer cancel()

	if err := h.sm.WaitForIndex(ctx, index); err != nil {
		http.Error(w, "timed out waiting for Raft apply", http.StatusGatewayTimeout)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// raftKVGetResponse is the JSON body for GET /raft/kv/{key}.
type raftKVGetResponse struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// RaftKVGetHandler handles GET /raft/kv/{key}.
// It reads directly from the local KVStateMachine (not the WAL-backed KVStore).
type RaftKVGetHandler struct {
	sm *infraraft.KVStateMachine
}

// NewRaftKVGetHandler creates a RaftKVGetHandler.
func NewRaftKVGetHandler(sm *infraraft.KVStateMachine) *RaftKVGetHandler {
	return &RaftKVGetHandler{sm: sm}
}

// ServeHTTP handles GET /raft/kv/{key}.
func (h *RaftKVGetHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if key == "" {
		http.Error(w, "key is required", http.StatusBadRequest)
		return
	}

	value, ok := h.sm.Get(key)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(raftKVGetResponse{Key: key, Value: value}) //nolint:errcheck
}
