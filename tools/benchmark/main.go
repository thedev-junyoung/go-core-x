// Command benchmark runs a phased performance comparison against a running Core-X instance.
//
// Usage:
//
//	# Build Core-X for each phase and run benchmark against each:
//	go run ./tools/benchmark/ --addr http://127.0.0.1:8080 --rps 5000 --duration 10s
//
// This tool produces a comparison report showing latency percentiles and
// throughput across phases. It does NOT start/stop the server — the caller
// is responsible for pointing --addr at the correct instance.
//
// For an automated phase comparison, use the provided Makefile target:
//
//	make bench-phases
//
// Design rationale:
//   - Each phase is tested with the same load parameters to ensure apples-to-apples
//     comparison. The only variable is the server implementation.
//   - Warmup period discards initial requests (connection pool cold start, JIT-like
//     effects in Go's runtime scheduler).
//   - Multiple runs per phase with median selection reduces variance from OS scheduling.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/junyoung/core-x/tools/loadgen"
)

func main() {
	var (
		addr        = flag.String("addr", "http://127.0.0.1:8080", "Target server address (base URL)")
		rps         = flag.Int("rps", 5000, "Target RPS (0 = open-loop max throughput)")
		concurrency = flag.Int("concurrency", 50, "Number of concurrent workers")
		duration    = flag.Duration("duration", 15*time.Second, "Test duration per phase")
		warmup      = flag.Duration("warmup", 3*time.Second, "Warmup duration before measurement")
		phase       = flag.String("phase", "", "Phase label for the report (e.g. 'Phase 1: In-Memory')")
		openLoop    = flag.Bool("open-loop", false, "Run open-loop (max throughput) instead of fixed RPS")
		payload     = flag.String("payload", `{"source":"benchmark","payload":"phase-comparison-payload"}`, "JSON payload")
	)
	flag.Parse()

	targetRPS := *rps
	if *openLoop {
		targetRPS = 0
	}

	phaseLabel := *phase
	if phaseLabel == "" {
		phaseLabel = fmt.Sprintf("Benchmark (%s)", *addr)
	}

	ingestURL := *addr + "/ingest"

	fmt.Fprintf(os.Stdout, "\nCore-X Phase Benchmark\n")
	fmt.Fprintf(os.Stdout, "  Target : %s\n", ingestURL)
	fmt.Fprintf(os.Stdout, "  Phase  : %s\n", phaseLabel)
	if targetRPS > 0 {
		fmt.Fprintf(os.Stdout, "  Mode   : closed-loop (target %d RPS)\n", targetRPS)
	} else {
		fmt.Fprintf(os.Stdout, "  Mode   : open-loop (max throughput)\n")
	}
	fmt.Fprintf(os.Stdout, "  Workers: %d\n", *concurrency)
	fmt.Fprintf(os.Stdout, "  Warmup : %s\n", *warmup)
	fmt.Fprintf(os.Stdout, "  Run    : %s\n\n", *duration)

	ctx := context.Background()

	// --- Warmup phase ---
	// Sends requests but discards metrics. Ensures:
	//   1. HTTP keep-alive connections are established.
	//   2. Go runtime scheduler is warmed up.
	//   3. Server worker pool is primed.
	fmt.Fprintf(os.Stdout, "  [warming up for %s...]\n", *warmup)
	warmupGen := loadgen.New(loadgen.Config{
		TargetURL:   ingestURL,
		RPS:         targetRPS,
		Concurrency: *concurrency,
		Duration:    *warmup,
		Payload:     *payload,
		Timeout:     5 * time.Second,
	})
	_ = warmupGen.Run(ctx)
	fmt.Fprintln(os.Stdout, "  [warmup complete]")

	// --- Measurement phase ---
	fmt.Fprintf(os.Stdout, "  [measuring for %s...]\n", *duration)
	gen := loadgen.New(loadgen.Config{
		TargetURL:   ingestURL,
		RPS:         targetRPS,
		Concurrency: *concurrency,
		Duration:    *duration,
		Payload:     *payload,
		Timeout:     5 * time.Second,
	})

	result := gen.Run(ctx)

	pr := loadgen.PhaseResult{
		Phase:       phaseLabel,
		TargetRPS:   targetRPS,
		Concurrency: *concurrency,
		Result:      result,
	}
	loadgen.PrintReport(os.Stdout, pr)

	// Exit non-zero if error rate exceeds 1% (for CI use).
	if result.ErrorRate > 0.01 {
		fmt.Fprintf(os.Stderr, "WARNING: error rate %.2f%% exceeds 1%% threshold\n",
			result.ErrorRate*100)
		os.Exit(1)
	}
}
