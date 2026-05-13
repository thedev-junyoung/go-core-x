package http

// MVCCKVHandler and CASHandler implement the ADR-022 MVCC HTTP layer.
//
// Endpoints:
//   - GET  /mvcc/kv/{key}             → latest value (linearizable via ReadIndex)
//   - GET  /mvcc/kv/{key}?version=N   → snapshot read at version N (INV-MV5)
//   - PUT  /mvcc/kv/{key}             → CAS write; body {"value":"…","expected_version":N}
//
// Invariants enforced here:
//   - INV-MV3: CAS conflict is signalled as 409; IsCASConflict returns the
//     version that was current at conflict time.
//   - INV-MV4: expected_version==0 is an unconditional write (no CAS check).
//   - INV-MV5: ?version=N reads bypass ReadIndex entirely.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	infraraft "github.com/junyoung/core-x/internal/infrastructure/raft"
)

const (
	mvccReadIndexTimeout    = 2 * time.Second
	mvccWaitForIndexTimeout = 2 * time.Second
	mvccIngestTimeout       = 5 * time.Second
)

// casWriteRequest is the JSON body for PUT /mvcc/kv/{key}.
type casWriteRequest struct {
	Value           string `json:"value"`
	ExpectedVersion uint64 `json:"expected_version"` // 0 = unconditional write (INV-MV4)
}

// casConflictResponse is the JSON body for 409 Conflict responses.
type casConflictResponse struct {
	CurrentVersion uint64 `json:"current_version"`
}

// mvccGetResponse is the JSON body for GET /mvcc/kv/{key} responses.
type mvccGetResponse struct {
	Key     string `json:"key"`
	Value   string `json:"value"`
	Version uint64 `json:"version"`
}

// MVCCKVHandler handles GET /mvcc/kv/{key} and GET /mvcc/kv/{key}?version=N.
type MVCCKVHandler struct {
	registry *infraraft.RaftGroupRegistry
	sm       *infraraft.MVCCStateMachine
	selfID   string
	addrMap  map[string]string
}

// NewMVCCKVHandler creates a MVCCKVHandler. registry and sm must be non-nil.
func NewMVCCKVHandler(
	registry *infraraft.RaftGroupRegistry,
	sm *infraraft.MVCCStateMachine,
	selfID string,
	addrMap map[string]string,
) *MVCCKVHandler {
	if registry == nil {
		panic("http: NewMVCCKVHandler requires non-nil registry")
	}
	if sm == nil {
		panic("http: NewMVCCKVHandler requires non-nil sm")
	}
	return &MVCCKVHandler{registry: registry, sm: sm, selfID: selfID, addrMap: addrMap}
}

// ServeHTTP handles GET /mvcc/kv/{key} and GET /mvcc/kv/{key}?version=N.
func (h *MVCCKVHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if key == "" {
		http.Error(w, "key is required", http.StatusBadRequest)
		return
	}

	// INV-MV5: snapshot read — bypass ReadIndex entirely.
	if rawVer := r.URL.Query().Get("version"); rawVer != "" {
		ver, err := strconv.ParseUint(rawVer, 10, 64)
		if err != nil || ver == 0 {
			http.Error(w, "version must be a positive integer", http.StatusBadRequest)
			return
		}
		value, found := h.sm.GetVersion(key, ver)
		if !found {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(mvccGetResponse{Key: key, Value: value, Version: ver}) //nolint:errcheck
		return
	}

	// Linearizable read: ReadIndex → WaitForIndex → sm.Get.
	partitionID := infraraft.PartitionID(h.selfID)
	node, ok := h.registry.Get(partitionID)
	if !ok {
		http.Error(w, "raft partition unavailable", http.StatusServiceUnavailable)
		return
	}

	readCtx, readCancel := context.WithTimeout(r.Context(), mvccReadIndexTimeout)
	defer readCancel()

	readIndex, err := node.ReadIndex(readCtx)
	if err != nil {
		if errors.Is(err, infraraft.ErrNotLeader) {
			if h.addrMap != nil {
				if leaderID := node.LeaderID(); leaderID != "" {
					if baseURL, ok := h.addrMap[leaderID]; ok {
						http.Redirect(w, r, baseURL+r.URL.RequestURI(), http.StatusTemporaryRedirect)
						return
					}
				}
			}
			http.Error(w, "not the Raft leader — retry on the leader node", http.StatusServiceUnavailable)
			return
		}
		http.Error(w, "read index unavailable", http.StatusServiceUnavailable)
		return
	}

	waitCtx, waitCancel := context.WithTimeout(r.Context(), mvccWaitForIndexTimeout)
	defer waitCancel()

	if err := h.sm.WaitForIndex(waitCtx, int64(readIndex)); err != nil {
		if errors.Is(err, infraraft.ErrSnapshotDisplaced) {
			// Snapshot installed mid-request; state is consistent but we can't
			// attribute individual CAS results. Client retries get fresh state.
			http.Error(w, "snapshot installed — retry", http.StatusServiceUnavailable)
			return
		}
		http.Error(w, "timed out waiting for state machine", http.StatusGatewayTimeout)
		return
	}

	// FIX-2: CAS conflict flood can leave lastApplied < readIndex even after
	// WaitForIndex returns (conflict entries do not advance lastApplied).
	// A read at readIndex must not see data older than that index.
	if h.sm.LastApplied() < int64(readIndex) {
		http.Error(w, "state machine lagging — retry", http.StatusServiceUnavailable)
		return
	}

	value, version, found := h.sm.Get(key)
	if !found {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(mvccGetResponse{Key: key, Value: value, Version: version}) //nolint:errcheck
}

// CASHandler handles PUT /mvcc/kv/{key} — compare-and-swap writes.
//
// Body: {"value":"…","expected_version":N}
// N==0 → unconditional write (INV-MV4); 204 on success.
// N>0  → CAS: 204 on success, 409 Conflict on version mismatch (INV-MV3).
// Not-leader → 307 redirect (if addrMap set) or 503.
type CASHandler struct {
	registry *infraraft.RaftGroupRegistry
	sm       *infraraft.MVCCStateMachine
	selfID   string
	addrMap  map[string]string
}

// NewCASHandler creates a CASHandler. registry and sm must be non-nil.
func NewCASHandler(
	registry *infraraft.RaftGroupRegistry,
	sm *infraraft.MVCCStateMachine,
	selfID string,
	addrMap map[string]string,
) *CASHandler {
	if registry == nil {
		panic("http: NewCASHandler requires non-nil registry")
	}
	if sm == nil {
		panic("http: NewCASHandler requires non-nil sm")
	}
	return &CASHandler{registry: registry, sm: sm, selfID: selfID, addrMap: addrMap}
}

// ServeHTTP handles PUT /mvcc/kv/{key}.
func (h *CASHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if key == "" {
		http.Error(w, "key is required", http.StatusBadRequest)
		return
	}

	var req casWriteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	op := "set"
	if req.ExpectedVersion > 0 {
		op = "cas"
	}
	cmd := infraraft.RaftKVCommand{
		Op:              op,
		Key:             key,
		Value:           req.Value,
		ExpectedVersion: req.ExpectedVersion,
	}
	data, err := json.Marshal(cmd)
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	partitionID := infraraft.PartitionID(h.selfID)
	node, ok := h.registry.Get(partitionID)
	if !ok {
		http.Error(w, "raft partition unavailable", http.StatusServiceUnavailable)
		return
	}

	index, _, isLeader := node.Propose(data)
	if !isLeader {
		if h.addrMap != nil {
			if leaderID := node.LeaderID(); leaderID != "" {
				if baseURL, ok := h.addrMap[leaderID]; ok {
					http.Redirect(w, r, baseURL+r.URL.RequestURI(), http.StatusTemporaryRedirect)
					return
				}
			}
		}
		http.Error(w, "not the Raft leader — retry on the leader node", http.StatusServiceUnavailable)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), mvccIngestTimeout)
	defer cancel()

	if err := h.sm.WaitForIndex(ctx, index); err != nil {
		if errors.Is(err, infraraft.ErrSnapshotDisplaced) {
			// FIX-1: snapshot installed before this entry was individually applied.
			// We cannot determine success vs conflict; client must retry.
			http.Error(w, "snapshot installed — retry", http.StatusServiceUnavailable)
			return
		}
		http.Error(w, "timed out waiting for Raft apply", http.StatusGatewayTimeout)
		return
	}

	// FIX-3: IsCASConflict returns the version that was current at conflict time
	// (stored atomically in conflictResults by apply()). This avoids the TOCTOU
	// race of a separate CurrentVersion() call after WaitForIndex returns.
	if ver, conflict := h.sm.IsCASConflict(index); conflict {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict) // 409
		json.NewEncoder(w).Encode(casConflictResponse{CurrentVersion: ver}) //nolint:errcheck
		return
	}

	w.WriteHeader(http.StatusNoContent) // 204
}
