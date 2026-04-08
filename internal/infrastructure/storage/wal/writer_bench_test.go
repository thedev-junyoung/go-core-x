package wal_test

import (
	"path/filepath"
	"testing"

	"github.com/junyoung/core-x/internal/domain"
	"github.com/junyoung/core-x/internal/infrastructure/storage/wal"
)

// BenchmarkWriter_Write_SyncNever는 SyncNever 정책일 때 Write의 성능을 측정한다.
//
// 목표: 0 allocs/op (pool 히트 시)
// 기준: > 500MB/s 처리량
func BenchmarkWriter_Write_SyncNever(b *testing.B) {
	tmpDir := b.TempDir()
	walPath := filepath.Join(tmpDir, "bench_write.wal")

	w, err := wal.NewWriter(wal.Config{
		Path:       walPath,
		SyncPolicy: wal.SyncNever,
	})
	if err != nil {
		b.Fatalf("failed to create writer: %v", err)
	}
	defer w.Close()

	// 테스트 페이로드
	testPayload := make([]byte, 100)
	for i := range testPayload {
		testPayload[i] = byte(i % 256)
	}

	// 워밍업: pool hit 상태로 진입
	for i := 0; i < 10; i++ {
		w.Write(testPayload)
	}

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if err := w.Write(testPayload); err != nil {
			b.Fatalf("write failed: %v", err)
		}
	}

	// 처리량 계산 (페이로드만, WAL 오버헤드 제외)
	totalBytes := int64(len(testPayload)) * int64(b.N)
	b.ReportMetric(float64(totalBytes)/1e6/b.Elapsed().Seconds(), "MB/sec")
}

// BenchmarkWriter_WriteEvent_SyncNever는 SyncNever 정책일 때 WriteEvent의 성능을 측정한다.
//
// 목표: 0 allocs/op (pool 히트 시)
// 기준: > 300MB/s 처리량 (Event 인코딩 오버헤드 포함)
func BenchmarkWriter_WriteEvent_SyncNever(b *testing.B) {
	tmpDir := b.TempDir()
	walPath := filepath.Join(tmpDir, "bench_write_event.wal")

	w, err := wal.NewWriter(wal.Config{
		Path:       walPath,
		SyncPolicy: wal.SyncNever,
	})
	if err != nil {
		b.Fatalf("failed to create writer: %v", err)
	}
	defer w.Close()

	// 테스트 이벤트
	testEvent := &domain.Event{
		Source:  "bench-source",
		Payload: "payload-data-for-benchmark-test",
	}

	// 워밍업: pool hit 상태로 진입
	for i := 0; i < 10; i++ {
		w.WriteEvent(testEvent)
	}

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if err := w.WriteEvent(testEvent); err != nil {
			b.Fatalf("write event failed: %v", err)
		}
	}

	// 처리량 계산
	eventSize := 2 + len(testEvent.Source) + 2 + len(testEvent.Payload)
	totalBytes := int64(eventSize) * int64(b.N)
	b.ReportMetric(float64(totalBytes)/1e6/b.Elapsed().Seconds(), "MB/sec")
}

// BenchmarkWriter_Write_SyncImmediate는 SyncImmediate 정책일 때의 성능을 측정한다.
//
// 목표: fsync 비용 측정 (일반적으로 5-20ms)
// 참고: SyncNever 대비 ~1000배 느림 (디스크 I/O 대기)
func BenchmarkWriter_Write_SyncImmediate(b *testing.B) {
	tmpDir := b.TempDir()
	walPath := filepath.Join(tmpDir, "bench_write_sync.wal")

	w, err := wal.NewWriter(wal.Config{
		Path:       walPath,
		SyncPolicy: wal.SyncImmediate,
	})
	if err != nil {
		b.Fatalf("failed to create writer: %v", err)
	}
	defer w.Close()

	testPayload := make([]byte, 100)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if err := w.Write(testPayload); err != nil {
			b.Fatalf("write failed: %v", err)
		}
	}
}

// BenchmarkWriter_PoolReuse는 pool 버퍼 재사용을 검증한다.
//
// 단일 Write로는 pool miss 가능성이 높으므로,
// N개 Write를 순차적으로 실행하면서 pool 히트율을 측정한다.
func BenchmarkWriter_PoolReuse(b *testing.B) {
	tmpDir := b.TempDir()
	walPath := filepath.Join(tmpDir, "bench_pool_reuse.wal")

	w, err := wal.NewWriter(wal.Config{
		Path:       walPath,
		SyncPolicy: wal.SyncNever,
	})
	if err != nil {
		b.Fatalf("failed to create writer: %v", err)
	}
	defer w.Close()

	testPayload := make([]byte, 100)

	// 워밍업
	for i := 0; i < 10; i++ {
		w.Write(testPayload)
	}

	b.ReportAllocs()
	b.ResetTimer()

	// N = b.N번의 Write를 반복 — pool 히트가 계속 발생해야 함
	for i := 0; i < b.N; i++ {
		if err := w.Write(testPayload); err != nil {
			b.Fatalf("write failed: %v", err)
		}
	}

	// 목표: allocs/op = 0 (pool hit 시)
}
