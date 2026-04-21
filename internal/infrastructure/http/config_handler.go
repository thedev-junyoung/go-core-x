package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	infraraft "github.com/junyoung/core-x/internal/infrastructure/raft"
)

const configChangeHTTPTimeout = 35 * time.Second

// configChangeRequest is the JSON body for POST /raft/config.
type configChangeRequest struct {
	Action string   `json:"action"` // "add" or "remove"
	Nodes  []string `json:"nodes"`  // peer gRPC addresses to add or remove
}

// ConfigHandler handles POST /raft/config.
//
// Flow:
//  1. Decode JSON body → action + node list
//  2. node.ComputeNewVoters(action, nodes) → new voter list
//  3. node.ProposeConfigChange(ctx, newVoters) → doneCh
//  4. Wait for doneCh (35s timeout)
//  5. 204 No Content on success, X-Raft-Applied-Index header set
//
// On ErrNotLeader: redirect to known leader (307) or 503.
// On ErrConfigChangeInProgress: 409 Conflict.
type ConfigHandler struct {
	node    *infraraft.RaftNode
	addrMap map[string]string // nodeID → HTTP base URL, may be nil
}

// NewConfigHandler creates a ConfigHandler.
// addrMap maps Raft node IDs to HTTP base URLs used for leader redirect.
// Pass nil to disable redirect.
func NewConfigHandler(node *infraraft.RaftNode, addrMap map[string]string) *ConfigHandler {
	return &ConfigHandler{node: node, addrMap: addrMap}
}

// ServeHTTP handles POST /raft/config.
func (h *ConfigHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req configChangeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.Action != "add" && req.Action != "remove" {
		http.Error(w, `"action" must be "add" or "remove"`, http.StatusBadRequest)
		return
	}
	if len(req.Nodes) == 0 {
		http.Error(w, `"nodes" must not be empty`, http.StatusBadRequest)
		return
	}

	// Compute the new voter list from the current config and the delta.
	newVoters, err := h.node.ComputeNewVoters(req.Action, req.Nodes)
	if err != nil {
		if errors.Is(err, infraraft.ErrConfigChangeInProgress) {
			http.Error(w, "config change already in progress", http.StatusConflict)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), configChangeHTTPTimeout)
	defer cancel()

	doneCh, err := h.node.ProposeConfigChange(ctx, newVoters)
	if err != nil {
		if errors.Is(err, infraraft.ErrNotLeader) {
			if leaderID := h.node.LeaderID(); leaderID != "" {
				if h.addrMap != nil {
					if baseURL, ok := h.addrMap[leaderID]; ok {
						http.Redirect(w, r, baseURL+"/raft/config", http.StatusTemporaryRedirect)
						return
					}
				}
			}
			http.Error(w, "not the Raft leader", http.StatusServiceUnavailable)
			return
		}
		if errors.Is(err, infraraft.ErrConfigChangeInProgress) {
			http.Error(w, "config change already in progress", http.StatusConflict)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	select {
	case appliedIndex, ok := <-doneCh:
		if !ok {
			http.Error(w, "config change failed or leader stepped down", http.StatusInternalServerError)
			return
		}
		w.Header().Set("X-Raft-Applied-Index", strconv.FormatInt(appliedIndex, 10))
		w.WriteHeader(http.StatusNoContent)
	case <-ctx.Done():
		http.Error(w, "config change timed out", http.StatusGatewayTimeout)
	}
}
