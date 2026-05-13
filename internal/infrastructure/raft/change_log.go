package raft

// ChangeLog implements the CDC (Change Data Capture) pub/sub hub (ADR-023).
//
// Design constraints:
//   - Slow consumers MUST NOT block apply() (backpressure isolation). (INV-CDC4)
//   - Goroutine leaks MUST NOT occur on Unsubscribe or ctx cancellation. (INV-CDC5)
//   - Publish MUST be non-blocking from the caller's perspective. (INV-CDC4)
//   - ReplayFrom(offset) returns ErrOffsetOutOfRange when offset < baseOffset. (INV-CDC6)
//
// Concurrency model:
//   - mu (RWMutex) protects subscribers, history, and baseOffset.
//   - nextID uses atomic.Uint64 — no lock needed for ID generation.
//   - Publish acquires RLock to snapshot subscriber list, then releases before sending.
//     Individual channel sends are lock-free (bounded buffer + non-blocking select).
//   - dropped and published counters are updated with atomic adds.

import (
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// ErrOffsetOutOfRange is returned by ReplayFrom when the requested offset has
// been GC'd (offset < baseOffset). Raft snapshot compaction is the primary cause.
// (INV-CDC6)
var ErrOffsetOutOfRange = errors.New("cdc: offset out of range — Raft snapshot GC'd this event")

// ErrChangeLogClosed is returned by Subscribe when the ChangeLog has been closed.
var ErrChangeLogClosed = errors.New("cdc: ChangeLog is closed")

// ChangeEvent represents a single committed state change in the MVCC store.
//
// Offset is the Raft log index that produced this event. Consumers use Offset
// to detect gaps, request replays, and implement exactly-once processing on
// their side.
//
// Version is the MVCC version assigned to the key by this write (ADR-022).
// For "del" events, Version is the tombstone version (last version before delete).
type ChangeEvent struct {
	Type      string    `json:"type"`      // "set" | "del"
	Key       string    `json:"key"`
	Value     string    `json:"value"`     // empty for "del"
	Version   uint64    `json:"version"`   // MVCC version (ADR-022)
	Offset    int64     `json:"offset"`    // Raft log index; monotonically increasing (INV-CDC2)
	Timestamp time.Time `json:"timestamp"` // wall clock at apply() time (informational only)
}

// subscription holds a single subscriber's state.
type subscription struct {
	id   uint64
	ch   chan ChangeEvent // bounded buffer (default 256); Publish uses non-blocking send
}

// ChangeLog manages CDC subscriptions and event fan-out.
//
// It maintains a bounded history ring buffer for offset-based replay (INV-CDC6).
// The history is in-memory only and is cleared on server restart.
type ChangeLog struct {
	mu          sync.RWMutex
	subscribers map[uint64]*subscription
	nextID      atomic.Uint64

	// history retains recent events for offset-based replay.
	// Capped at historyLimit entries. Oldest entries are evicted when full.
	history      []ChangeEvent
	historyLimit int

	// baseOffset is the Raft log index of the first event stored in history.
	// Events with offset < baseOffset have been GC'd and cannot be replayed. (INV-CDC6)
	baseOffset int64

	closed bool // set to true by Close(); checked by Subscribe

	// Prometheus metrics (nil-safe: all methods guard with if m != nil).
	metrics *cdcMetrics
}

// cdcMetrics groups all CDC Prometheus counters.
type cdcMetrics struct {
	publishedTotal  prometheus.Counter
	droppedTotal    *prometheus.CounterVec // label: subscriber_id
	subscriberCount prometheus.Gauge
}

// DefaultHistoryLimit is the default number of events retained in the replay buffer.
const DefaultHistoryLimit = 1000

// DefaultSubscriberBufSize is the recommended subscriber channel buffer depth.
// Burst absorption: at 10k events/s, 256 entries ≈ 25ms of headroom.
const DefaultSubscriberBufSize = 256

// NewChangeLog creates a ChangeLog with the given history limit and Prometheus registry.
// reg may be nil (metrics disabled).
func NewChangeLog(historyLimit int, reg *prometheus.Registry) *ChangeLog {
	if historyLimit <= 0 {
		historyLimit = DefaultHistoryLimit
	}

	cl := &ChangeLog{
		subscribers:  make(map[uint64]*subscription),
		history:      make([]ChangeEvent, 0, historyLimit),
		historyLimit: historyLimit,
	}

	if reg != nil {
		m := &cdcMetrics{
			publishedTotal: prometheus.NewCounter(prometheus.CounterOpts{
				Name: "cdc_events_published_total",
				Help: "Total number of CDC events published to subscribers.",
			}),
			droppedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
				Name: "cdc_events_dropped_total",
				Help: "Total number of CDC events dropped due to slow consumers.",
			}, []string{"subscriber_id"}),
			subscriberCount: prometheus.NewGauge(prometheus.GaugeOpts{
				Name: "cdc_subscribers_count",
				Help: "Current number of active CDC subscribers.",
			}),
		}
		reg.MustRegister(m.publishedTotal, m.droppedTotal, m.subscriberCount)
		cl.metrics = m
	}

	return cl
}

// Subscribe registers a new subscriber and returns its ID and a receive-only channel.
//
// bufSize controls the channel buffer depth. Use DefaultSubscriberBufSize (256) if unsure.
// The caller MUST call Unsubscribe(id) when done to avoid goroutine leaks. (INV-CDC5)
//
// Returns ErrChangeLogClosed if the ChangeLog has been shut down.
func (cl *ChangeLog) Subscribe(bufSize int) (id uint64, ch <-chan ChangeEvent, err error) {
	if bufSize <= 0 {
		bufSize = DefaultSubscriberBufSize
	}

	cl.mu.Lock()
	defer cl.mu.Unlock()

	if cl.closed {
		return 0, nil, ErrChangeLogClosed
	}

	id = cl.nextID.Add(1)
	sub := &subscription{
		id: id,
		ch: make(chan ChangeEvent, bufSize),
	}
	cl.subscribers[id] = sub

	if cl.metrics != nil {
		cl.metrics.subscriberCount.Set(float64(len(cl.subscribers)))
	}

	slog.Debug("cdc: new subscriber", "id", id, "buf_size", bufSize)
	return id, sub.ch, nil
}

// Unsubscribe removes a subscriber by ID and closes its channel.
// Safe to call from any goroutine. Idempotent — calling twice is safe. (INV-CDC5)
func (cl *ChangeLog) Unsubscribe(id uint64) {
	cl.mu.Lock()
	sub, ok := cl.subscribers[id]
	if ok {
		delete(cl.subscribers, id)
		close(sub.ch)
		if cl.metrics != nil {
			cl.metrics.subscriberCount.Set(float64(len(cl.subscribers)))
		}
	}
	cl.mu.Unlock()

	if ok {
		slog.Debug("cdc: subscriber removed", "id", id)
	}
}

// Publish delivers ev to all registered subscribers and appends it to the history buffer.
//
// Publish is non-blocking: if a subscriber's channel buffer is full the event is dropped
// for that subscriber and cdc_events_dropped_total is incremented. (INV-CDC4)
//
// Publish is called from apply() — it must never block that goroutine.
func (cl *ChangeLog) Publish(ev ChangeEvent) {
	// --- Append to history (write lock, brief) ---
	cl.mu.Lock()
	if cl.closed {
		cl.mu.Unlock()
		return
	}
	if len(cl.history) == 0 {
		cl.baseOffset = ev.Offset
	}
	cl.history = append(cl.history, ev)
	// Evict oldest events when history is full (ring buffer semantics).
	if len(cl.history) > cl.historyLimit {
		cl.history = cl.history[1:] // evict oldest; baseOffset advances
		cl.baseOffset = cl.history[0].Offset
	}
	// Snapshot subscriber list under the same lock to avoid separate RLock/RUnlock.
	subs := make([]*subscription, 0, len(cl.subscribers))
	for _, s := range cl.subscribers {
		subs = append(subs, s)
	}
	cl.mu.Unlock()

	// --- Fan-out (lock-free, non-blocking) ---
	for _, s := range subs {
		select {
		case s.ch <- ev:
			// delivered
		default:
			// slow consumer: drop event and record metric (INV-CDC4)
			slog.Debug("cdc: slow consumer drop", "subscriber_id", s.id, "offset", ev.Offset)
			if cl.metrics != nil {
				// CounterVec WithLabelValues allocation is acceptable here (non-hot path
				// only when drops occur; normal path never reaches default branch).
				cl.metrics.droppedTotal.WithLabelValues(formatUint64(s.id)).Inc()
			}
		}
	}

	if cl.metrics != nil {
		cl.metrics.publishedTotal.Inc()
	}
}

// ReplayFrom returns all history events with Offset >= startOffset.
//
// Returns ErrOffsetOutOfRange if startOffset < baseOffset (events GC'd). (INV-CDC6)
func (cl *ChangeLog) ReplayFrom(startOffset int64) ([]ChangeEvent, error) {
	cl.mu.RLock()
	defer cl.mu.RUnlock()

	if len(cl.history) == 0 {
		// No events yet — return empty slice, not an error.
		return nil, nil
	}

	if startOffset < cl.baseOffset {
		return nil, ErrOffsetOutOfRange
	}

	// Binary search would be O(log n) but history is bounded at 1000; linear is fine.
	var result []ChangeEvent
	for _, ev := range cl.history {
		if ev.Offset >= startOffset {
			result = append(result, ev)
		}
	}
	return result, nil
}

// BaseOffset returns the smallest Offset available in the history buffer.
// Returns -1 if the history is empty.
func (cl *ChangeLog) BaseOffset() int64 {
	cl.mu.RLock()
	defer cl.mu.RUnlock()
	if len(cl.history) == 0 {
		return -1
	}
	return cl.baseOffset
}

// LatestOffset returns the Offset of the most recently published event.
// Returns -1 if no events have been published.
func (cl *ChangeLog) LatestOffset() int64 {
	cl.mu.RLock()
	defer cl.mu.RUnlock()
	if len(cl.history) == 0 {
		return -1
	}
	return cl.history[len(cl.history)-1].Offset
}

// SubscriberCount returns the current number of active subscribers.
func (cl *ChangeLog) SubscriberCount() int {
	cl.mu.RLock()
	defer cl.mu.RUnlock()
	return len(cl.subscribers)
}

// Close shuts down the ChangeLog: closes all subscriber channels (unblocking range loops)
// and prevents new subscriptions. Idempotent. (INV-CDC5)
func (cl *ChangeLog) Close() {
	cl.mu.Lock()
	defer cl.mu.Unlock()

	if cl.closed {
		return
	}
	cl.closed = true

	for id, sub := range cl.subscribers {
		close(sub.ch)
		delete(cl.subscribers, id)
	}

	if cl.metrics != nil {
		cl.metrics.subscriberCount.Set(0)
	}

	slog.Info("cdc: ChangeLog closed")
}

// formatUint64 converts a uint64 to its decimal string representation.
// Used for Prometheus label values to avoid fmt.Sprintf allocation on the hot path.
// (This is only called on slow-consumer drops, not the normal fast path.)
func formatUint64(n uint64) string {
	if n == 0 {
		return "0"
	}
	// Maximum uint64 decimal digits: 20.
	var buf [20]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte(n%10) + '0'
		n /= 10
	}
	return string(buf[pos:])
}
