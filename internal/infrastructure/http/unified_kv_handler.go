package http

// UnifiedKVHandler handles GET /kv/{key} via the Raft ReadIndex protocol
// when CORE_X_STORAGE_UNIFIED=true (ADR-021 Step 3, Phase 12a).
//
// Invariants:
//   - INV-UKV1 (Linearizable read): every read first obtains a readIndex via
//     ReadIndex(ctx), then waits for the state machine to apply up to that
//     index before accessing sm.Get(). This prevents stale reads on a leader
//     that has been partitioned.
//   - INV-UKV2 (Owner routing): the ring decides ownership. Requests for keys
//     owned by a peer are forwarded via gRPC (identical to the legacy KVHandler
//     routing). Only owner-local reads go through the Raft path.
//   - INV-UKV3 (No nil): registry and sm must be non-nil; enforced in
//     NewUnifiedKVHandler with a panic.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/junyoung/core-x/internal/domain"
	"github.com/junyoung/core-x/internal/infrastructure/cluster"
	infragrpc "github.com/junyoung/core-x/internal/infrastructure/grpc"
	infraraft "github.com/junyoung/core-x/internal/infrastructure/raft"
)

// unifiedReadIndexTimeout caps the ReadIndex RPC (leadership confirmation + quorum round-trip).
// unifiedWaitForIndexTimeout caps WaitForIndex (state machine apply lag).
// Each phase gets its own independent budget so a slow ReadIndex does not
// silently starve WaitForIndex, causing spurious 504s.
const (
	unifiedReadIndexTimeout    = 2 * time.Second
	unifiedWaitForIndexTimeout = 2 * time.Second
)

// UnifiedKVHandler handles GET /kv/{key} via Raft ReadIndex.
//
// It replaces KVHandler in the HTTP mux when CORE_X_STORAGE_UNIFIED=true.
// The forwarding logic for non-owner keys is identical to KVHandler.
type UnifiedKVHandler struct {
	registry       *infraraft.RaftGroupRegistry
	sm             *infraraft.KVStateMachine
	selfID         string
	ring           *cluster.Ring        // nil in single-node mode
	forwarder      *infragrpc.Forwarder // nil in single-node mode
	forwardTimeout time.Duration
	addrMap        map[string]string // nodeID → HTTP base URL for leader redirect; may be nil
}

// NewUnifiedKVHandler creates a UnifiedKVHandler.
//
// registry and sm must be non-nil. ring and forwarder may be nil in single-node
// mode (no peer forwarding). addrMap may be nil (leader redirect disabled).
func NewUnifiedKVHandler(
	registry *infraraft.RaftGroupRegistry,
	sm *infraraft.KVStateMachine,
	selfID string,
	ring *cluster.Ring,
	forwarder *infragrpc.Forwarder,
	forwardTimeout time.Duration,
	addrMap map[string]string,
) *UnifiedKVHandler {
	if registry == nil {
		panic("http: NewUnifiedKVHandler requires non-nil registry")
	}
	if sm == nil {
		panic("http: NewUnifiedKVHandler requires non-nil sm")
	}
	if forwardTimeout <= 0 {
		forwardTimeout = 3 * time.Second
	}
	return &UnifiedKVHandler{
		registry:       registry,
		sm:             sm,
		selfID:         selfID,
		ring:           ring,
		forwarder:      forwarder,
		forwardTimeout: forwardTimeout,
		addrMap:        addrMap,
	}
}

// ServeHTTP handles GET /kv/{key}.
//
// Read path when the local node owns the key:
//  1. registry.Get(partitionID) — look up the owning RaftNode
//  2. RaftNode.ReadIndex(ctx) — confirm leadership; obtain safe read index
//  3. KVStateMachine.WaitForIndex(ctx, readIndex) — state machine catches up
//  4. sm.Get(key) — read from now-consistent local state
//
// On ErrNotLeader: redirect to the known leader (307) or 503.
// On ReadIndex timeout: 503.
// On WaitForIndex timeout: 504.
func (h *UnifiedKVHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if key == "" {
		http.Error(w, "key is required", http.StatusBadRequest)
		return
	}

	// INV-UKV2: ring-based forwarding for non-owner keys (unchanged from KVHandler).
	if h.ring != nil {
		if target, ok := h.ring.Lookup(key); ok && target.ID != h.selfID {
			if !target.IsHealthy() {
				http.Error(w, "target node unavailable", http.StatusServiceUnavailable)
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), h.forwardTimeout)
			defer cancel()

			resp, err := h.forwarder.ForwardGet(ctx, target, key)
			if err != nil {
				http.Error(w, "target node unavailable", http.StatusServiceUnavailable)
				return
			}
			if !resp.Found {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			writeKVJSON(w, &domain.Event{
				Source:     resp.Source,
				Payload:    string(resp.Payload),
				ReceivedAt: time.Unix(0, resp.ReceivedAtUnixNs),
			})
			return
		}
	}

	// INV-UKV1: owner-local read via Raft ReadIndex.
	partitionID := infraraft.PartitionID(h.selfID)
	node, ok := h.registry.Get(partitionID)
	if !ok {
		http.Error(w, "raft partition unavailable", http.StatusServiceUnavailable)
		return
	}

	// Phase 1: confirm leadership and obtain a safe read index.
	// Uses its own deadline so it does not consume time from the WaitForIndex budget.
	readCtx, readCancel := context.WithTimeout(r.Context(), unifiedReadIndexTimeout)
	defer readCancel()

	readIndex, err := node.ReadIndex(readCtx)
	if err != nil {
		if errors.Is(err, infraraft.ErrNotLeader) {
			if leaderID := node.LeaderID(); leaderID != "" {
				if baseURL, ok := h.addrMap[leaderID]; ok {
					http.Redirect(w, r, baseURL+r.URL.RequestURI(), http.StatusTemporaryRedirect)
					return
				}
			}
			http.Error(w, "not the Raft leader — retry on the leader node", http.StatusServiceUnavailable)
			return
		}
		// ErrReadIndexTimeout or other transient failure.
		http.Error(w, "read index unavailable", http.StatusServiceUnavailable)
		return
	}

	// Phase 2: wait for the state machine to apply up to readIndex.
	// Independent deadline ensures apply-lag under load does not inherit a
	// depleted context from Phase 1.
	waitCtx, waitCancel := context.WithTimeout(r.Context(), unifiedWaitForIndexTimeout)
	defer waitCancel()

	if err := h.sm.WaitForIndex(waitCtx, int64(readIndex)); err != nil {
		http.Error(w, "timed out waiting for state machine", http.StatusGatewayTimeout)
		return
	}

	value, found := h.sm.Get(key)
	if !found {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(raftKVGetResponse{Key: key, Value: value}) //nolint:errcheck
}
