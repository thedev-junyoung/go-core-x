package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/junyoung/core-x/internal/domain"
	"github.com/junyoung/core-x/internal/infrastructure/cluster"
	infragrpc "github.com/junyoung/core-x/internal/infrastructure/grpc"
	kvstore "github.com/junyoung/core-x/internal/infrastructure/storage/kv"
)

// KVStoreGetter abstracts KV point-lookups for the HTTP layer.
type KVStoreGetter interface {
	Get(key string) (*domain.Event, error)
}

// KVHandler handles GET /kv/{key}.
//
// Routing logic (mirrors write path):
//   - ring != nil && key owned by peer → ForwardGet to peer
//   - ring != nil && key owned by self → local kvStore.Get
//   - ring == nil (single-node) → local kvStore.Get
type KVHandler struct {
	kv             KVStoreGetter
	ring           *cluster.Ring
	selfID         string
	forwarder      *infragrpc.Forwarder
	forwardTimeout time.Duration
}

// NewKVHandler creates a KVHandler. Pass nil ring for single-node mode.
func NewKVHandler(
	kv KVStoreGetter,
	ring *cluster.Ring,
	selfID string,
	forwarder *infragrpc.Forwarder,
) *KVHandler {
	return &KVHandler{
		kv:             kv,
		ring:           ring,
		selfID:         selfID,
		forwarder:      forwarder,
		forwardTimeout: 3 * time.Second,
	}
}

// kvResponse is the JSON wire format for GET /kv/{key}.
type kvResponse struct {
	Source           string `json:"source"`
	Payload          string `json:"payload"`
	ReceivedAtUnixNs int64  `json:"received_at_unix_ns"`
}

// ServeHTTP handles GET /kv/{key}.
func (h *KVHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if key == "" {
		http.Error(w, "key is required", http.StatusBadRequest)
		return
	}

	// Cluster mode: route by consistent hash.
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

	// Local lookup.
	event, err := h.kv.Get(key)
	if err != nil {
		if errors.Is(err, kvstore.ErrKeyNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	writeKVJSON(w, event)
}

func writeKVJSON(w http.ResponseWriter, e *domain.Event) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(kvResponse{ //nolint:errcheck
		Source:           e.Source,
		Payload:          e.Payload,
		ReceivedAtUnixNs: e.ReceivedAt.UnixNano(),
	})
}
