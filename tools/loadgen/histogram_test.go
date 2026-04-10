package loadgen

import (
	"testing"
	"time"
)

func TestHistogram_Record_and_Percentile(t *testing.T) {
	h := &Histogram{}

	// Record 100 samples at known latencies.
	// 50 at 1ms, 40 at 10ms, 10 at 100ms.
	for i := 0; i < 50; i++ {
		h.Record(time.Millisecond)
	}
	for i := 0; i < 40; i++ {
		h.Record(10 * time.Millisecond)
	}
	for i := 0; i < 10; i++ {
		h.Record(100 * time.Millisecond)
	}

	if h.Count() != 100 {
		t.Errorf("Count() = %d, want 100", h.Count())
	}

	// p50 should be in the 1ms bucket (first 50% of values).
	p50 := h.Percentile(50)
	if p50 < time.Millisecond || p50 > 4*time.Millisecond {
		t.Errorf("p50 = %s, want ~1ms (within power-of-2 bucket)", p50)
	}

	// p90 (50+40=90th percentile) should be in the 10ms range.
	p90 := h.Percentile(90)
	if p90 < 8*time.Millisecond || p90 > 20*time.Millisecond {
		t.Errorf("p90 = %s, want ~10ms (within power-of-2 bucket)", p90)
	}

	// p99 should be in the 100ms range.
	p99 := h.Percentile(99)
	if p99 < 64*time.Millisecond || p99 > 256*time.Millisecond {
		t.Errorf("p99 = %s, want ~100ms (within power-of-2 bucket)", p99)
	}
}

func TestHistogram_Reset(t *testing.T) {
	h := &Histogram{}
	h.Record(time.Millisecond)
	h.Record(time.Second)

	if h.Count() != 2 {
		t.Fatalf("expected count=2, got %d", h.Count())
	}

	h.Reset()

	if h.Count() != 0 {
		t.Errorf("after Reset, Count() = %d, want 0", h.Count())
	}
	if h.Percentile(99) != 0 {
		t.Errorf("after Reset, p99 = %s, want 0", h.Percentile(99))
	}
}

func TestHistogram_Empty(t *testing.T) {
	h := &Histogram{}
	if h.Percentile(99) != 0 {
		t.Errorf("empty histogram p99 = %s, want 0", h.Percentile(99))
	}
	if h.Mean() != 0 {
		t.Errorf("empty histogram mean = %s, want 0", h.Mean())
	}
}

func BenchmarkHistogram_Record(b *testing.B) {
	h := &Histogram{}
	d := 500 * time.Microsecond
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.Record(d)
	}
}
