package raft_test

// change_log_test.go — Unit tests for ChangeLog (ADR-023).
//
// Coverage:
//   - Subscribe → Publish → receive
//   - Multi-subscriber fan-out
//   - Slow consumer drop (buffer full) → drop counter, other subscribers unaffected
//   - Unsubscribe → channel closed, no further events received
//   - ReplayFrom: events returned for offset >= startOffset
//   - ReplayFrom(offset < baseOffset) → ErrOffsetOutOfRange
//   - Close: all subscriber channels closed; Subscribe after Close returns ErrChangeLogClosed
//   - Goroutine leak: manual verification (no goleak dependency)

import (
	"testing"
	"time"

	. "github.com/junyoung/core-x/internal/infrastructure/raft"
)

// makeEvent creates a ChangeEvent for testing.
func makeEvent(offset int64, key, value string, version uint64) ChangeEvent {
	return ChangeEvent{
		Type:      "set",
		Key:       key,
		Value:     value,
		Version:   version,
		Offset:    offset,
		Timestamp: time.Now(),
	}
}

// TestChangeLog_SubscribePublishReceive verifies that a subscriber receives
// an event published after subscription.
func TestChangeLog_SubscribePublishReceive(t *testing.T) {
	cl := NewChangeLog(100, nil)
	defer cl.Close()

	id, ch, err := cl.Subscribe(16)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cl.Unsubscribe(id)

	ev := makeEvent(1, "k", "v", 1)
	cl.Publish(ev)

	select {
	case got := <-ch:
		if got.Key != ev.Key || got.Value != ev.Value || got.Offset != ev.Offset {
			t.Errorf("received wrong event: got %+v, want %+v", got, ev)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout waiting for event")
	}
}

// TestChangeLog_FanOut verifies that multiple subscribers each receive every event.
func TestChangeLog_FanOut(t *testing.T) {
	cl := NewChangeLog(100, nil)
	defer cl.Close()

	const n = 5
	ids := make([]uint64, n)
	chs := make([]<-chan ChangeEvent, n)
	for i := range n {
		id, ch, err := cl.Subscribe(32)
		if err != nil {
			t.Fatalf("Subscribe[%d]: %v", i, err)
		}
		ids[i] = id
		chs[i] = ch
	}
	defer func() {
		for _, id := range ids {
			cl.Unsubscribe(id)
		}
	}()

	events := []ChangeEvent{
		makeEvent(1, "a", "1", 1),
		makeEvent(2, "b", "2", 1),
		makeEvent(3, "c", "3", 1),
	}
	for _, ev := range events {
		cl.Publish(ev)
	}

	for i, ch := range chs {
		for _, want := range events {
			select {
			case got := <-ch:
				if got.Offset != want.Offset {
					t.Errorf("subscriber %d: got offset %d, want %d", i, got.Offset, want.Offset)
				}
			case <-time.After(100 * time.Millisecond):
				t.Fatalf("subscriber %d: timeout waiting for offset %d", i, want.Offset)
			}
		}
	}
}

// TestChangeLog_SlowConsumer verifies that a slow consumer (buffer full) causes
// drops while other subscribers with available buffer space are not affected.
func TestChangeLog_SlowConsumer(t *testing.T) {
	cl := NewChangeLog(100, nil)
	defer cl.Close()

	// slow: buffer of 1; will fill up quickly.
	slowID, slowCh, err := cl.Subscribe(1)
	if err != nil {
		t.Fatalf("Subscribe slow: %v", err)
	}
	defer cl.Unsubscribe(slowID)

	// fast: large buffer; should receive all events.
	fastID, fastCh, err := cl.Subscribe(64)
	if err != nil {
		t.Fatalf("Subscribe fast: %v", err)
	}
	defer cl.Unsubscribe(fastID)

	// Publish 10 events without draining slow.
	const total = 10
	for i := range int64(total) {
		cl.Publish(makeEvent(i+1, "k", "v", uint64(i+1)))
	}

	// Fast subscriber should receive all events.
	received := 0
	deadline := time.After(200 * time.Millisecond)
	for {
		select {
		case <-fastCh:
			received++
			if received == total {
				goto fastDone
			}
		case <-deadline:
			t.Fatalf("fast subscriber: received only %d/%d events before timeout", received, total)
		}
	}
fastDone:

	// Slow subscriber: at least 1 received (buffer=1); rest were dropped.
	// We just verify it didn't block publish or affect the fast subscriber.
	// Drain whatever is in the slow buffer (at most 1).
	drained := 0
drainLoop:
	for {
		select {
		case <-slowCh:
			drained++
		default:
			break drainLoop
		}
	}
	// slow subscriber received at most bufSize=1 event.
	if drained > 1 {
		t.Errorf("slow subscriber drained %d events with buf=1 (impossible without blocking)", drained)
	}
}

// TestChangeLog_Unsubscribe verifies that after Unsubscribe, the channel is
// closed and no more events are delivered.
func TestChangeLog_Unsubscribe(t *testing.T) {
	cl := NewChangeLog(100, nil)
	defer cl.Close()

	id, ch, err := cl.Subscribe(16)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Publish one event, receive it, then unsubscribe.
	cl.Publish(makeEvent(1, "k", "v", 1))
	select {
	case <-ch:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout waiting for first event")
	}

	cl.Unsubscribe(id)

	// Channel must be closed after Unsubscribe.
	select {
	case _, open := <-ch:
		if open {
			t.Fatal("channel still open after Unsubscribe — expected close")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("channel not closed after Unsubscribe")
	}

	// Idempotent: calling Unsubscribe again must not panic.
	cl.Unsubscribe(id)
}

// TestChangeLog_ReplayFrom_Normal verifies that ReplayFrom returns events
// with Offset >= startOffset.
func TestChangeLog_ReplayFrom_Normal(t *testing.T) {
	cl := NewChangeLog(100, nil)
	defer cl.Close()

	for i := range int64(10) {
		cl.Publish(makeEvent(i+1, "k", "v", uint64(i+1)))
	}

	// Replay from offset 5 — should get events 5..10 (6 events).
	events, err := cl.ReplayFrom(5)
	if err != nil {
		t.Fatalf("ReplayFrom(5): %v", err)
	}
	if len(events) != 6 {
		t.Fatalf("expected 6 events, got %d", len(events))
	}
	for i, ev := range events {
		want := int64(5 + i)
		if ev.Offset != want {
			t.Errorf("events[%d].Offset = %d, want %d", i, ev.Offset, want)
		}
	}
}

// TestChangeLog_ReplayFrom_OutOfRange verifies ErrOffsetOutOfRange when
// startOffset < baseOffset (events GC'd).
func TestChangeLog_ReplayFrom_OutOfRange(t *testing.T) {
	// historyLimit=3: after 5 publishes, the first 2 are evicted.
	cl := NewChangeLog(3, nil)
	defer cl.Close()

	for i := range int64(5) {
		cl.Publish(makeEvent(i+1, "k", "v", uint64(i+1)))
	}

	// baseOffset should now be 3 (events 1, 2 evicted).
	base := cl.BaseOffset()
	if base != 3 {
		t.Fatalf("expected baseOffset=3, got %d", base)
	}

	_, err := cl.ReplayFrom(1)
	if err != ErrOffsetOutOfRange {
		t.Fatalf("expected ErrOffsetOutOfRange, got %v", err)
	}
}

// TestChangeLog_ReplayFrom_Empty verifies that ReplayFrom on an empty ChangeLog
// returns nil slice with no error.
func TestChangeLog_ReplayFrom_Empty(t *testing.T) {
	cl := NewChangeLog(100, nil)
	defer cl.Close()

	events, err := cl.ReplayFrom(0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("expected empty slice, got %d events", len(events))
	}
}

// TestChangeLog_Close_SubscribersUnblocked verifies that Close() closes all
// subscriber channels, allowing goroutines blocked on range to exit.
func TestChangeLog_Close_SubscribersUnblocked(t *testing.T) {
	cl := NewChangeLog(100, nil)

	id, ch, err := cl.Subscribe(4)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	_ = id

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range ch {
			// drain
		}
		// channel closed — goroutine exits
	}()

	cl.Publish(makeEvent(1, "k", "v", 1))
	cl.Close()

	select {
	case <-done:
		// goroutine exited cleanly after channel close (INV-CDC5)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("goroutine did not exit after Close — possible leak")
	}
}

// TestChangeLog_SubscribeAfterClose verifies that Subscribe returns ErrChangeLogClosed
// after Close() has been called.
func TestChangeLog_SubscribeAfterClose(t *testing.T) {
	cl := NewChangeLog(100, nil)
	cl.Close()

	_, _, err := cl.Subscribe(16)
	if err != ErrChangeLogClosed {
		t.Fatalf("expected ErrChangeLogClosed, got %v", err)
	}
}

// TestChangeLog_SubscriberCount verifies that SubscriberCount tracks correctly.
func TestChangeLog_SubscriberCount(t *testing.T) {
	cl := NewChangeLog(100, nil)
	defer cl.Close()

	if n := cl.SubscriberCount(); n != 0 {
		t.Fatalf("initial count: got %d, want 0", n)
	}

	id1, _, _ := cl.Subscribe(8)
	id2, _, _ := cl.Subscribe(8)
	if n := cl.SubscriberCount(); n != 2 {
		t.Fatalf("after 2 subscribes: got %d, want 2", n)
	}

	cl.Unsubscribe(id1)
	if n := cl.SubscriberCount(); n != 1 {
		t.Fatalf("after 1 unsubscribe: got %d, want 1", n)
	}

	cl.Unsubscribe(id2)
	if n := cl.SubscriberCount(); n != 0 {
		t.Fatalf("after all unsubscribes: got %d, want 0", n)
	}
}

// TestChangeLog_LatestAndBaseOffset verifies offset tracking helpers.
func TestChangeLog_LatestAndBaseOffset(t *testing.T) {
	cl := NewChangeLog(3, nil)
	defer cl.Close()

	// No events: both return -1.
	if b := cl.BaseOffset(); b != -1 {
		t.Errorf("BaseOffset empty: got %d, want -1", b)
	}
	if l := cl.LatestOffset(); l != -1 {
		t.Errorf("LatestOffset empty: got %d, want -1", l)
	}

	cl.Publish(makeEvent(10, "k", "v", 1))
	if b := cl.BaseOffset(); b != 10 {
		t.Errorf("BaseOffset after 1 publish: got %d, want 10", b)
	}
	if l := cl.LatestOffset(); l != 10 {
		t.Errorf("LatestOffset after 1 publish: got %d, want 10", l)
	}

	// Fill history (limit=3) and evict the first.
	cl.Publish(makeEvent(11, "k", "v", 2))
	cl.Publish(makeEvent(12, "k", "v", 3))
	cl.Publish(makeEvent(13, "k", "v", 4)) // evicts offset 10

	if b := cl.BaseOffset(); b != 11 {
		t.Errorf("BaseOffset after eviction: got %d, want 11", b)
	}
	if l := cl.LatestOffset(); l != 13 {
		t.Errorf("LatestOffset after eviction: got %d, want 13", l)
	}
}

// TestChangeLog_Close_Idempotent verifies that calling Close twice does not panic.
func TestChangeLog_Close_Idempotent(t *testing.T) {
	cl := NewChangeLog(100, nil)
	cl.Close()
	cl.Close() // must not panic
}

// BenchmarkChangeLog_Publish measures publish throughput with N subscribers.
// Baseline expectation: ≥500k events/s with 1 subscriber on M1.
func BenchmarkChangeLog_Publish_1Sub(b *testing.B) {
	benchmarkPublish(b, 1)
}

func BenchmarkChangeLog_Publish_10Sub(b *testing.B) {
	benchmarkPublish(b, 10)
}

func benchmarkPublish(b *testing.B, numSubs int) {
	b.Helper()
	cl := NewChangeLog(DefaultHistoryLimit, nil)
	defer cl.Close()

	// Start drain goroutines so channels don't fill up during bench.
	for range numSubs {
		_, ch, _ := cl.Subscribe(DefaultSubscriberBufSize)
		go func() {
			for range ch {
			}
		}()
	}

	ev := makeEvent(1, "bench-key", "bench-val", 1)
	b.ResetTimer()
	b.ReportAllocs()
	for range b.N {
		cl.Publish(ev)
	}
}
