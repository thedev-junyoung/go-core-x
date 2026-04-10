package loadgen

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// PhaseResult associates a named phase with its load test result.
type PhaseResult struct {
	Phase       string // e.g. "Phase 1: In-Memory", "Phase 2: Hash Index KV"
	TargetRPS   int
	Concurrency int
	Result      Result
}

// PrintReport writes a single-phase result in human-readable tabular format.
func PrintReport(w io.Writer, pr PhaseResult) {
	sep := strings.Repeat("─", 60)
	fmt.Fprintln(w, sep)
	fmt.Fprintf(w, "  %s\n", pr.Phase)
	fmt.Fprintln(w, sep)
	fmt.Fprintf(w, "  Target RPS    : %d\n", pr.TargetRPS)
	fmt.Fprintf(w, "  Concurrency   : %d\n", pr.Concurrency)
	fmt.Fprintf(w, "  Duration      : %s\n", pr.Result.Duration.Round(time.Millisecond))
	fmt.Fprintf(w, "  Total Requests: %d\n", pr.Result.TotalRequests)
	fmt.Fprintf(w, "  Actual RPS    : %.1f\n", pr.Result.RPS)
	fmt.Fprintln(w, "")
	fmt.Fprintf(w, "  Successes (2xx): %d\n", pr.Result.Successes)
	fmt.Fprintf(w, "  Failures       : %d  (error rate: %.2f%%)\n",
		pr.Result.Failures, pr.Result.ErrorRate*100)
	fmt.Fprintf(w, "    503 (node down / overload): %d\n", pr.Result.Errors503)
	fmt.Fprintf(w, "    429 (worker pool saturated): %d\n", pr.Result.Errors429)
	fmt.Fprintf(w, "    network errors             : %d\n", pr.Result.NetworkErrors)
	fmt.Fprintln(w, "")
	fmt.Fprintf(w, "  Latency (client-side, includes TCP setup):\n")
	fmt.Fprintf(w, "    Mean : %s\n", fmtDuration(pr.Result.Mean))
	fmt.Fprintf(w, "    p50  : %s\n", fmtDuration(pr.Result.P50))
	fmt.Fprintf(w, "    p95  : %s\n", fmtDuration(pr.Result.P95))
	fmt.Fprintf(w, "    p99  : %s\n", fmtDuration(pr.Result.P99))
	fmt.Fprintln(w, sep)
}

// PrintComparisonReport prints a side-by-side comparison of multiple phase results.
//
// Output format (example):
//
//	Phase                   | RPS (actual) | p50    | p95    | p99    | Error%
//	────────────────────────┼──────────────┼────────┼────────┼────────┼────────
//	Phase 1: In-Memory      |    9823.4    |  0.1ms |  0.5ms |  1.2ms |  0.00%
//	Phase 2: Hash Index KV  |    9641.2    |  0.2ms |  0.8ms |  2.1ms |  0.01%
func PrintComparisonReport(w io.Writer, results []PhaseResult) {
	if len(results) == 0 {
		return
	}

	header := fmt.Sprintf("%-28s | %-12s | %-7s | %-7s | %-7s | %-8s | %-8s",
		"Phase", "RPS (actual)", "Mean", "p50", "p95", "p99", "Errors")
	divider := strings.Repeat("─", len(header))

	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "  PHASE COMPARISON REPORT")
	fmt.Fprintln(w, "  "+divider)
	fmt.Fprintln(w, "  "+header)
	fmt.Fprintln(w, "  "+divider)

	for _, pr := range results {
		fmt.Fprintf(w,
			"  %-28s | %12.1f | %-7s | %-7s | %-7s | %-8s | %.2f%%\n",
			pr.Phase,
			pr.Result.RPS,
			fmtDuration(pr.Result.Mean),
			fmtDuration(pr.Result.P50),
			fmtDuration(pr.Result.P95),
			fmtDuration(pr.Result.P99),
			pr.Result.ErrorRate*100,
		)
	}
	fmt.Fprintln(w, "  "+divider)
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "  Notes:")
	fmt.Fprintln(w, "    - Latency is client-side (includes connection setup)")
	fmt.Fprintln(w, "    - p99 uses power-of-two histogram buckets (±50% precision at tail)")
	fmt.Fprintln(w, "    - 503: node unavailable / overloaded")
	fmt.Fprintln(w, "    - 429: worker pool saturated (expected under overload)")
	fmt.Fprintln(w, "")
}

// fmtDuration formats a duration for table display: uses ms for >= 1ms, µs otherwise.
func fmtDuration(d time.Duration) string {
	if d >= time.Millisecond {
		return fmt.Sprintf("%.2fms", float64(d)/float64(time.Millisecond))
	}
	return fmt.Sprintf("%dµs", d.Microseconds())
}
