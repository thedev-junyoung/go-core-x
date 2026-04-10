// Package loadgen implements a high-throughput HTTP load generator for Core-X.
//
// Design philosophy (DDIA Reliability + Scalability):
//   - The measurement tool must not become the bottleneck. Measurement overhead
//     must be sub-microsecond on the hot path to avoid probe effect.
//   - Worker pool topology: fixed-size goroutine pool (concurrency param) feeding
//     a rate-limiter ticker. No goroutine explosion. Backpressure via blocking send.
//   - All counters use atomic.Int64 — lock-free, cache-line aware, no mutex contention.
//   - Latency histogram uses power-of-two bucket boundaries: O(1) record, O(1) percentile.
package loadgen

import (
	"math/bits"
	"sync/atomic"
	"time"
)

// histogramBuckets is the number of buckets in the latency histogram.
// Buckets are powers of two in microseconds: [0,1), [1,2), [2,4), ... [2^N, ∞).
// 32 buckets covers 0 → ~4 seconds in microsecond resolution.
const histogramBuckets = 32

// Histogram is a lock-free latency histogram using power-of-two bucket boundaries.
//
// Bucket i stores counts for latencies in [2^(i-1) µs, 2^i µs).
// Bucket 0 is a special "sub-1µs" bucket.
//
// Why power-of-two boundaries?
//   - O(1) record: bits.Len64(microseconds) → bucket index, no division, no branch table.
//   - O(1) percentile: scan 32 buckets, accumulate counts.
//   - Zero allocations after construction.
//
// Trade-off: logarithmic precision (not linear). Acceptable for p99 reporting
// where ±50% bucket width is within noise at tail latencies.
type Histogram struct {
	buckets [histogramBuckets]atomic.Int64
	count   atomic.Int64
	totalUs atomic.Int64 // for mean calculation
}

// Record adds a single observation to the histogram.
// Safe for concurrent use. Zero heap allocations.
func (h *Histogram) Record(d time.Duration) {
	us := d.Microseconds()
	if us < 0 {
		us = 0
	}
	h.count.Add(1)
	h.totalUs.Add(us)

	// Bucket index = number of bits needed to represent µs value.
	// us=0 → 0 bits → bucket 0
	// us=1 → 1 bit  → bucket 1
	// us=1023 → 10 bits → bucket 10 (~1ms)
	// us=1048575 → 20 bits → bucket 20 (~1s)
	idx := bits.Len64(uint64(us))
	if idx >= histogramBuckets {
		idx = histogramBuckets - 1
	}
	h.buckets[idx].Add(1)
}

// Percentile returns the approximate latency at the given percentile (0.0–100.0).
// Scans buckets linearly — O(32) = effectively O(1).
func (h *Histogram) Percentile(pct float64) time.Duration {
	total := h.count.Load()
	if total == 0 {
		return 0
	}

	target := int64(float64(total) * pct / 100.0)
	if target <= 0 {
		target = 1
	}

	var cum int64
	for i := 0; i < histogramBuckets; i++ {
		cum += h.buckets[i].Load()
		if cum >= target {
			// Upper bound of bucket i is 2^i µs.
			// Return the upper bound as a conservative estimate.
			if i == 0 {
				return time.Microsecond
			}
			upperUs := int64(1) << i
			return time.Duration(upperUs) * time.Microsecond
		}
	}
	return time.Duration(int64(1)<<(histogramBuckets-1)) * time.Microsecond
}

// Count returns the total number of observations.
func (h *Histogram) Count() int64 {
	return h.count.Load()
}

// Mean returns the arithmetic mean latency.
func (h *Histogram) Mean() time.Duration {
	n := h.count.Load()
	if n == 0 {
		return 0
	}
	return time.Duration(h.totalUs.Load()/n) * time.Microsecond
}

// Reset zeroes all buckets and counters atomically.
// Not perfectly atomic across all buckets — intended for between-run resets only.
func (h *Histogram) Reset() {
	for i := range h.buckets {
		h.buckets[i].Store(0)
	}
	h.count.Store(0)
	h.totalUs.Store(0)
}
