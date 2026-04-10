package loadgen_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/junyoung/core-x/tools/loadgen"
)

// TestGenerator_BasicRun verifies the generator sends requests and collects metrics.
// Uses httptest.Server as the target — no real server needed.
func TestGenerator_BasicRun(t *testing.T) {
	var received atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		w.WriteHeader(http.StatusAccepted) // 202
	}))
	defer srv.Close()

	gen := loadgen.New(loadgen.Config{
		TargetURL:   srv.URL + "/ingest",
		RPS:         500,
		Concurrency: 10,
		Duration:    2 * time.Second,
		Timeout:     1 * time.Second,
	})

	result := gen.Run(context.Background())

	if result.TotalRequests == 0 {
		t.Error("expected at least one request")
	}
	if result.Successes != result.TotalRequests {
		t.Errorf("expected all successes, got %d/%d", result.Successes, result.TotalRequests)
	}
	if result.ErrorRate > 0 {
		t.Errorf("expected 0 error rate, got %.4f", result.ErrorRate)
	}
	if result.RPS < 10 {
		t.Errorf("RPS too low: %.1f", result.RPS)
	}
	if result.P99 == 0 {
		t.Error("p99 should not be zero")
	}
	t.Logf("Result: requests=%d rps=%.1f p50=%s p99=%s",
		result.TotalRequests, result.RPS, result.P50, result.P99)
}

// TestGenerator_OpenLoop verifies open-loop mode saturates the server.
func TestGenerator_OpenLoop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	gen := loadgen.New(loadgen.Config{
		TargetURL:   srv.URL + "/ingest",
		RPS:         0, // open-loop
		Concurrency: 20,
		Duration:    1 * time.Second,
		Timeout:     1 * time.Second,
	})

	result := gen.Run(context.Background())
	// Open-loop should achieve significantly higher RPS than 500.
	if result.RPS < 1000 {
		t.Errorf("open-loop RPS too low: %.1f (httptest.Server should handle >>1000)", result.RPS)
	}
	t.Logf("Open-loop RPS: %.1f", result.RPS)
}

// TestGenerator_503Counting verifies 503 responses are counted correctly.
func TestGenerator_503Counting(t *testing.T) {
	var count atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := count.Add(1)
		// Every other request returns 503.
		if n%2 == 0 {
			w.WriteHeader(http.StatusServiceUnavailable)
		} else {
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	defer srv.Close()

	gen := loadgen.New(loadgen.Config{
		TargetURL:   srv.URL + "/ingest",
		RPS:         200,
		Concurrency: 5,
		Duration:    1 * time.Second,
		Timeout:     1 * time.Second,
	})

	result := gen.Run(context.Background())

	if result.TotalRequests == 0 {
		t.Fatal("no requests sent")
	}
	// ~50% should be 503.
	ratio := float64(result.Errors503) / float64(result.TotalRequests)
	if ratio < 0.3 || ratio > 0.7 {
		t.Errorf("expected ~50%% 503s, got %.1f%% (%d/%d)",
			ratio*100, result.Errors503, result.TotalRequests)
	}
	t.Logf("503 rate: %.1f%% (%d/%d)", ratio*100, result.Errors503, result.TotalRequests)
}
