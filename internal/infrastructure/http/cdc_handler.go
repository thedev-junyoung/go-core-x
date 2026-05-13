package http

// CDCHandler implements the CDC SSE endpoint (ADR-023).
//
// Endpoints:
//   - GET /cdc/stream              → live SSE stream from latest offset
//   - GET /cdc/stream?offset=N     → replay events from offset N, then live
//   - GET /cdc/status              → JSON: baseOffset, latestOffset, subscribers
//
// SSE format (text/event-stream):
//
//	id: <offset>\n
//	event: change\n
//	data: <json>\n\n
//
// Goroutine leak prevention:
//   - defer cl.Unsubscribe(id) is always called when the handler exits.
//   - r.Context().Done() detects client disconnect.
//   - ChangeLog.Close() (server shutdown) closes subscriber channels → for-range exits.

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	infraraft "github.com/junyoung/core-x/internal/infrastructure/raft"
)

// CDCHandler handles GET /cdc/stream and GET /cdc/status.
type CDCHandler struct {
	changeLog *infraraft.ChangeLog
}

// NewCDCHandler creates a CDCHandler. changeLog must be non-nil.
func NewCDCHandler(cl *infraraft.ChangeLog) *CDCHandler {
	if cl == nil {
		panic("http: NewCDCHandler requires non-nil ChangeLog")
	}
	return &CDCHandler{changeLog: cl}
}

// ServeHTTP dispatches to the stream or status sub-handler based on path suffix.
// Register this handler for "GET /cdc/stream" and "GET /cdc/status" separately,
// or wrap with a mux that routes on the full path.
func (h *CDCHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/cdc/status":
		h.serveStatus(w, r)
	default:
		h.serveStream(w, r)
	}
}

// serveStream handles GET /cdc/stream.
//
// Query params:
//   - ?offset=N  replay events with Offset >= N, then switch to live tail.
//
// Request headers:
//   - Last-Event-ID: N  browser EventSource reconnect; equivalent to ?offset=N+1
//     (replays from N+1 to avoid re-delivering the last seen event).
func (h *CDCHandler) serveStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported by this server", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx proxy buffering for SSE

	// SSE is a long-lived stream. The server's default WriteTimeout (10s)
	// would force-close idle connections. Disable per-connection write deadline
	// so the stream lives until the client disconnects or the server shuts down.
	// http.ResponseController is Go 1.20+; SetWriteDeadline(zero) clears the deadline.
	if err := http.NewResponseController(w).SetWriteDeadline(time.Time{}); err != nil {
		// Not fatal — older transports may not support deadline control. Continue
		// and rely on the configured WriteTimeout. Log for diagnosability.
		slog.Warn("cdc: failed to clear write deadline; long streams may be cut by WriteTimeout", "err", err)
	}

	// Determine replay start offset.
	// Priority: ?offset query param > Last-Event-ID header > no replay.
	var replayOffset int64 = -1

	if rawOffset := r.URL.Query().Get("offset"); rawOffset != "" {
		off, err := strconv.ParseInt(rawOffset, 10, 64)
		if err != nil || off < 0 {
			http.Error(w, "offset must be a non-negative integer", http.StatusBadRequest)
			return
		}
		replayOffset = off
	} else if lastID := r.Header.Get("Last-Event-ID"); lastID != "" {
		// Browser EventSource reconnect: Last-Event-ID is the last received offset.
		// Replay from lastID+1 to avoid re-delivering the event the client already saw.
		off, err := strconv.ParseInt(lastID, 10, 64)
		if err == nil && off >= 0 {
			replayOffset = off + 1
		}
	}

	// Offset replay (snapshot range only). (INV-CDC6)
	if replayOffset >= 0 {
		events, err := h.changeLog.ReplayFrom(replayOffset)
		if err != nil {
			// INV-CDC6: offset < baseOffset → GC'd by Raft snapshot.
			http.Error(w, "offset out of range: "+err.Error(), http.StatusBadRequest)
			return
		}
		for _, ev := range events {
			writeSSEEvent(w, ev)
		}
		flusher.Flush()
	}

	// Live subscription. (INV-CDC5: defer Unsubscribe prevents goroutine leaks)
	id, ch, err := h.changeLog.Subscribe(infraraft.DefaultSubscriberBufSize)
	if err != nil {
		// ChangeLog was closed (server shutting down).
		http.Error(w, "CDC stream unavailable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer h.changeLog.Unsubscribe(id)

	slog.Debug("cdc: SSE client connected", "subscriber_id", id, "remote", r.RemoteAddr)

	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				// Channel closed by ChangeLog.Close() (server shutdown). (INV-CDC5)
				slog.Debug("cdc: ChangeLog closed; SSE handler exiting", "subscriber_id", id)
				return
			}
			writeSSEEvent(w, ev)
			flusher.Flush()
		case <-r.Context().Done():
			// Client disconnected or request context cancelled. (INV-CDC5)
			slog.Debug("cdc: client disconnected", "subscriber_id", id, "remote", r.RemoteAddr)
			return
		}
	}
}

// cdcStatusResponse is the JSON body for GET /cdc/status.
type cdcStatusResponse struct {
	BaseOffset      int64 `json:"base_offset"`    // -1 if no events published yet
	LatestOffset    int64 `json:"latest_offset"`  // -1 if no events published yet
	SubscriberCount int   `json:"subscriber_count"`
}

// serveStatus handles GET /cdc/status.
func (h *CDCHandler) serveStatus(w http.ResponseWriter, _ *http.Request) {
	resp := cdcStatusResponse{
		BaseOffset:      h.changeLog.BaseOffset(),
		LatestOffset:    h.changeLog.LatestOffset(),
		SubscriberCount: h.changeLog.SubscriberCount(),
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp) //nolint:errcheck
}

// writeSSEEvent writes a single SSE event to w.
//
// SSE format (RFC 8895 / text/event-stream):
//
//	id: <offset>
//	event: change
//	data: <json>
//	(blank line)
//
// The caller is responsible for calling flusher.Flush() after one or more events.
func writeSSEEvent(w http.ResponseWriter, ev infraraft.ChangeEvent) {
	data, err := json.Marshal(ev)
	if err != nil {
		// Malformed event — log and skip. Should never happen with ChangeEvent struct.
		slog.Error("cdc: failed to marshal ChangeEvent", "offset", ev.Offset, "err", err)
		return
	}
	fmt.Fprintf(w, "id: %d\nevent: change\ndata: %s\n\n", ev.Offset, data) //nolint:errcheck
}
