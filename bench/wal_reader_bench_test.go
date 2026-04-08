// bench/wal_reader_bench_test.go — WAL Reader performance benchmarks
//
// These benchmarks validate ADR-004 performance targets:
// - Sequential read throughput: > 100M bytes/sec
// - RoundTrip (write → read → decode): < 100 ns/record overhead
// - Recovery (truncation handling): graceful, O(file_size) latency
package bench

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/junyoung/core-x/internal/domain"
	"github.com/junyoung/core-x/internal/infrastructure/storage/wal"
)

// BenchmarkReader_Sequential measures throughput of reading an entire WAL file sequentially.
//
// Target: > 100M bytes/sec (sufficient for crash recovery)
// Setup: Creates a WAL file with N records, reads back all records.
func BenchmarkReader_Sequential(b *testing.B) {
	// Pre-create a WAL file with 1000 records (~256 KB total)
	tmpDir := b.TempDir()
	walPath := filepath.Join(tmpDir, "bench.wal")

	// Write 1000 test records
	writer, err := wal.NewWriter(wal.Config{
		Path:       walPath,
		SyncPolicy: wal.SyncNever, // Fast write for benchmark setup
	})
	if err != nil {
		b.Fatalf("failed to create writer: %v", err)
	}

	for i := 0; i < 1000; i++ {
		e := &domain.Event{
			Source:  "benchmark-source",
			Payload: "x", // minimal payload
		}
		payload := wal.EncodeEvent(e)
		if err := writer.Write(payload); err != nil {
			b.Fatalf("write failed: %v", err)
		}
	}
	writer.Close()

	// Get file size for throughput calculation
	info, _ := os.Stat(walPath)
	fileSize := info.Size()

	// Reset timer after setup
	b.ResetTimer()

	// Benchmark: read entire file N times
	for i := 0; i < b.N; i++ {
		r, err := wal.NewReader(walPath)
		if err != nil {
			b.Fatalf("failed to open reader: %v", err)
		}

		recordCount := 0
		for r.Scan() {
			recordCount++
		}
		if err := r.Err(); err != nil {
			b.Fatalf("read error: %v", err)
		}
		r.Close()

		if recordCount != 1000 {
			b.Fatalf("expected 1000 records, got %d", recordCount)
		}
	}

	// Report throughput
	b.ReportMetric(float64(fileSize)*float64(b.N)/1e6/b.Elapsed().Seconds(), "MB/sec")
}

// BenchmarkRoundTrip_WriteThenRead measures end-to-end latency of write-then-read-then-decode.
//
// Target: < 100 ns/record overhead (beyond I/O), alloc/op < 1
// Validates that Reader is efficient at recovering Events.
func BenchmarkRoundTrip_WriteThenRead(b *testing.B) {
	tmpDir := b.TempDir()

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		// Create a new WAL file for this iteration
		walPath := filepath.Join(tmpDir, "roundtrip_"+string(rune(i))+".wal")

		// Write
		writer, _ := wal.NewWriter(wal.Config{
			Path:       walPath,
			SyncPolicy: wal.SyncNever,
		})

		testEvent := &domain.Event{
			Source:  "source-value",
			Payload: "payload-data",
		}
		payload := wal.EncodeEvent(testEvent)
		writer.Write(payload)
		writer.Close()

		// Read
		reader, _ := wal.NewReader(walPath)
		if !reader.Scan() {
			b.Fatalf("scan failed")
		}

		rec := reader.Record()

		// Decode
		decoded, err := wal.DecodeEvent(rec.Data)
		if err != nil {
			b.Fatalf("decode failed: %v", err)
		}

		// Verify
		if decoded.Source != testEvent.Source || decoded.Payload != testEvent.Payload {
			b.Fatalf("mismatch: source %s vs %s, payload %s vs %s",
				decoded.Source, testEvent.Source, decoded.Payload, testEvent.Payload)
		}

		reader.Close()
	}
}

// BenchmarkReader_TruncationRecovery measures performance of recovering from tail truncation.
//
// Setup: Creates a WAL file with 100 complete records + 1 incomplete record.
// Benchmark: Verify Reader recovers the 100 complete records.
// Target: O(file_size) latency, graceful ErrTruncated return.
func BenchmarkReader_TruncationRecovery(b *testing.B) {
	tmpDir := b.TempDir()
	walPath := filepath.Join(tmpDir, "truncation_test.wal")

	// Write 100 complete records
	writer, _ := wal.NewWriter(wal.Config{
		Path:       walPath,
		SyncPolicy: wal.SyncNever,
	})

	for i := 0; i < 100; i++ {
		e := &domain.Event{
			Source:  "complete",
			Payload: "record",
		}
		payload := wal.EncodeEvent(e)
		writer.Write(payload)
	}
	writer.Close()

	// Truncate the file: remove last 10 bytes (incomplete record)
	f, _ := os.OpenFile(walPath, os.O_WRONLY, 0600)
	info, _ := f.Stat()
	f.Truncate(info.Size() - 10)
	f.Close()

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		r, _ := wal.NewReader(walPath)

		recovered := 0
		for r.Scan() {
			recovered++
		}

		// Should have recovered 100 records (last incomplete one skipped)
		if recovered != 100 {
			b.Fatalf("expected 100 recovered records, got %d", recovered)
		}

		// Should end with ErrTruncated
		if err := r.Err(); err == nil {
			b.Fatalf("expected ErrTruncated, got nil")
		}

		r.Close()
	}
}

// BenchmarkDecodeEvent_EdgeCases measures decoding performance on edge cases.
//
// Validates DecodeEvent is efficient even with empty/large source/payload.
func BenchmarkDecodeEvent_EdgeCases(b *testing.B) {
	tests := []struct {
		name    string
		event   *domain.Event
	}{
		{"EmptyBoth", &domain.Event{Source: "", Payload: ""}},
		{"EmptySource", &domain.Event{Source: "", Payload: "x"}},
		{"EmptyPayload", &domain.Event{Source: "x", Payload: ""}},
		{"Normal", &domain.Event{Source: "src", Payload: "payload"}},
		{"LargePayload", &domain.Event{Source: "s", Payload: string(make([]byte, 1000))}},
	}

	for _, tt := range tests {
		b.Run(tt.name, func(b *testing.B) {
			payload := wal.EncodeEvent(tt.event)

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				decoded, err := wal.DecodeEvent(payload)
				if err != nil {
					b.Fatalf("decode failed: %v", err)
				}
				if decoded.Source != tt.event.Source || decoded.Payload != tt.event.Payload {
					b.Fatalf("mismatch")
				}
			}
		})
	}
}
