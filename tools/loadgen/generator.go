package loadgen

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Config holds the load generator parameters.
//
// Concurrency controls the number of concurrent HTTP workers (goroutines).
// RPS controls the request rate. If RPS <= 0, the generator runs as fast as
// possible (open loop — useful for finding throughput ceiling).
//
// Open loop vs closed loop:
//   - Closed loop (fixed concurrency, no rate limit): measures max throughput.
//     Latency will spike as server saturates — models a DDoS-like scenario.
//   - Open loop (fixed RPS): models real traffic. Latency reflects true queuing.
//     Preferred for SLO validation (e.g., "p99 < 10ms at 5k RPS").
//
// Use open loop for SLO tests, closed loop for saturation tests.
type Config struct {
	TargetURL   string
	RPS         int           // 0 = open-loop max throughput
	Concurrency int           // number of parallel workers
	Duration    time.Duration // how long to run
	Payload     string        // JSON body to POST
	Timeout     time.Duration // per-request HTTP timeout
}

// Result holds the metrics collected during a load test run.
type Result struct {
	Duration      time.Duration
	TotalRequests int64
	Successes     int64  // 2xx responses
	Failures      int64  // non-2xx or error
	Errors503     int64  // service unavailable (overload / node down)
	Errors429     int64  // too many requests (worker pool saturated)
	NetworkErrors int64  // connection refused, timeout, etc.
	RPS           float64
	ErrorRate     float64 // 0.0 – 1.0
	P50           time.Duration
	P95           time.Duration
	P99           time.Duration
	Mean          time.Duration
}

// Generator is a fixed-pool HTTP load generator.
//
// Goroutine topology:
//   - 1 rate limiter goroutine (ticker → tokenCh, or no-op if open loop)
//   - N worker goroutines (drain tokenCh, send HTTP, record latency)
//   - 1 aggregator (reads from resultCh after run completes)
//
// No goroutine is created per request — only at Start() time.
// This bounds goroutine count to Concurrency+2 regardless of RPS.
type Generator struct {
	cfg     Config
	client  *http.Client
	bufPool sync.Pool // reuses *bytes.Buffer for request bodies
}

// New creates a Generator. The HTTP client is tuned for high concurrency:
//   - MaxIdleConnsPerHost = concurrency (avoid connection churn)
//   - DisableKeepAlives = false (reuse TCP connections)
//   - Timeout = cfg.Timeout (or 5s default)
func New(cfg Config) *Generator {
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 50
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Second
	}
	if cfg.Payload == "" {
		cfg.Payload = `{"source":"loadgen","payload":"benchmark-payload"}`
	}

	transport := &http.Transport{
		MaxIdleConnsPerHost:   cfg.Concurrency,
		MaxConnsPerHost:       cfg.Concurrency,
		IdleConnTimeout:       90 * time.Second,
		DisableKeepAlives:     false,
		ForceAttemptHTTP2:     false, // HTTP/1.1 has lower overhead for small requests
		ResponseHeaderTimeout: cfg.Timeout,
	}

	return &Generator{
		cfg: cfg,
		client: &http.Client{
			Transport: transport,
			Timeout:   cfg.Timeout,
		},
		bufPool: sync.Pool{
			New: func() any { return bytes.NewBuffer(make([]byte, 0, 256)) },
		},
	}
}

// Run executes the load test and returns aggregated metrics.
//
// Implementation notes:
//   - tokenCh acts as the rate-limiting gate. In open-loop mode it is unbuffered
//     and the sender never blocks (select default). In closed-loop mode, it is
//     buffered at concurrency depth so workers are always fed.
//   - Latency is measured from just before NewRequest to response body close.
//     This includes TCP connection time on cold starts — intentional, as it
//     reflects true client-perceived latency.
func (g *Generator) Run(ctx context.Context) Result {
	hist := &Histogram{}
	var (
		totalReqs    atomic.Int64
		successes    atomic.Int64
		failures     atomic.Int64
		errors503    atomic.Int64
		errors429    atomic.Int64
		networkErrs  atomic.Int64
	)

	runCtx, cancel := context.WithTimeout(ctx, g.cfg.Duration)
	defer cancel()

	// tokenCh: workers block here waiting for a send token.
	// Buffer = concurrency so workers can always get a token immediately when RPS
	// is high. The rate limiter fills it; workers drain it.
	var tokenCh chan struct{}
	if g.cfg.RPS > 0 {
		tokenCh = make(chan struct{}, g.cfg.Concurrency)
	} else {
		// Open-loop: always-ready channel trick — workers never block for a token.
		tokenCh = make(chan struct{}, g.cfg.Concurrency)
		// Pre-fill to max capacity; workers drain and we refill without rate limiting.
	}

	// Rate limiter goroutine.
	// Sends tokens at the configured RPS. Stops when runCtx is done.
	// Trade-off: time.NewTicker has ~100µs jitter on Linux. For very high RPS
	// (>100k), a token bucket with spinning would be more accurate.
	// At 10k RPS the jitter is acceptable (~1% of interval).
	if g.cfg.RPS > 0 {
		interval := time.Duration(float64(time.Second) / float64(g.cfg.RPS))
		go func() {
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-runCtx.Done():
					close(tokenCh)
					return
				case <-ticker.C:
					select {
					case tokenCh <- struct{}{}:
					default:
						// Worker pool is saturated; drop token (natural backpressure).
						// This shows up as lower actual RPS vs target — measure it.
					}
				}
			}
		}()
	} else {
		// Open-loop: close tokenCh only when duration expires.
		// Workers use a non-blocking select to drain.
		go func() {
			<-runCtx.Done()
			close(tokenCh)
		}()
	}

	start := time.Now()

	var wg sync.WaitGroup
	for i := 0; i < g.cfg.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if g.cfg.RPS > 0 {
					// Closed-loop: wait for rate limiter token.
					if _, ok := <-tokenCh; !ok {
						return
					}
				} else {
					// Open-loop: check context and fire immediately.
					select {
					case <-runCtx.Done():
						return
					default:
					}
				}

				// Measure from request construction to body close.
				reqStart := time.Now()
				statusCode, err := g.doRequest()
				elapsed := time.Since(reqStart)

				totalReqs.Add(1)
				hist.Record(elapsed)

				if err != nil {
					networkErrs.Add(1)
					failures.Add(1)
					continue
				}

				switch {
				case statusCode >= 200 && statusCode < 300:
					successes.Add(1)
				case statusCode == 503:
					errors503.Add(1)
					failures.Add(1)
				case statusCode == 429:
					errors429.Add(1)
					failures.Add(1)
				default:
					failures.Add(1)
				}
			}
		}()
	}

	wg.Wait()
	elapsed := time.Since(start)

	total := totalReqs.Load()
	fail := failures.Load()
	var errRate float64
	if total > 0 {
		errRate = float64(fail) / float64(total)
	}
	rps := float64(total) / elapsed.Seconds()

	return Result{
		Duration:      elapsed,
		TotalRequests: total,
		Successes:     successes.Load(),
		Failures:      fail,
		Errors503:     errors503.Load(),
		Errors429:     errors429.Load(),
		NetworkErrors: networkErrs.Load(),
		RPS:           rps,
		ErrorRate:     errRate,
		P50:           hist.Percentile(50),
		P95:           hist.Percentile(95),
		P99:           hist.Percentile(99),
		Mean:          hist.Mean(),
	}
}

// doRequest sends a single POST /ingest and returns (statusCode, error).
// Reuses a pooled bytes.Buffer to avoid a heap allocation per request body.
//
// Allocation budget per call:
//   - buf from pool: 0 alloc (pool hit after warmup)
//   - http.NewRequest: 1 alloc (internal context embedding)
//   - resp.Body.Close(): 0 alloc
//   - Net: ~1 alloc/request (irreducible with net/http stdlib)
func (g *Generator) doRequest() (int, error) {
	buf := g.bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	buf.WriteString(g.cfg.Payload)
	defer g.bufPool.Put(buf)

	req, err := http.NewRequest(http.MethodPost, g.cfg.TargetURL, buf)
	if err != nil {
		return 0, fmt.Errorf("loadgen: request construction failed: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.client.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}
