// Package bench_test는 Core-X stdlib 핸들러와 Gin 핸들러의 메모리 할당을 비교한다.
package bench_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	appingestion "github.com/junyoung/core-x/internal/application/ingestion"
	"github.com/junyoung/core-x/internal/domain"
	"github.com/junyoung/core-x/internal/infrastructure/executor"
	infrahttp "github.com/junyoung/core-x/internal/infrastructure/http"
	"github.com/junyoung/core-x/internal/infrastructure/pool"
)

// ingestReq는 POST /ingest 요청 본문 스키마다.
type ingestReq struct {
	Source  string `json:"source"`
	Payload string `json:"payload"`
}

// ginIngestHandler는 Gin 컨텍스트를 받아 처리하는 핸들러를 반환한다.
//
// Core-X의 stdlib 핸들러와 동일한 비즈니스 로직을 따른다:
//   - ShouldBindJSON으로 요청 파싱 (실패 시 400)
//   - source/payload 필수 필드 검증 (빈 값 시 422)
//   - svc.Ingest 호출
//   - ErrOverloaded 시 429, 그 외 500
//   - 성공 시 202
func ginIngestHandler(svc *appingestion.IngestionService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req ingestReq
		if err := c.ShouldBindJSON(&req); err != nil {
			c.Status(http.StatusBadRequest)
			return
		}
		if req.Source == "" || req.Payload == "" {
			c.Status(http.StatusUnprocessableEntity)
			return
		}
		if err := svc.Ingest(req.Source, req.Payload); err != nil {
			if errors.Is(err, appingestion.ErrOverloaded) {
				c.Status(http.StatusTooManyRequests)
				return
			}
			c.Status(http.StatusInternalServerError)
			return
		}
		c.Status(http.StatusAccepted)
	}
}

// newBenchInfra는 벤치마크용 IngestionService와 정리 함수를 반환한다.
//
// 두 벤치마크가 동일한 WorkerPool/EventPool을 사용해야 apples-to-apples 비교가 된다.
// no-op Processor를 사용해 비즈니스 로직 처리 비용을 제거한다.
func newBenchInfra() (*appingestion.IngestionService, func()) {
	ep := pool.New()
	wp := executor.NewWorkerPool(executor.Config{
		NumWorkers:    4,
		JobBufferSize: 1000,
		Processor:     domain.EventProcessorFunc(func(e *domain.Event) {}), // no-op
		EventPool:     ep,
	})
	svc := appingestion.NewIngestionService(wp, ep, nil) // nil for WALWriter (benchmark only)

	cleanup := func() {
		wp.Shutdown(context.Background())
	}
	return svc, cleanup
}

const benchBody = `{"source":"bench","payload":"hello-world"}`

// BenchmarkCoreXIngest는 stdlib net/http 핸들러의 메모리 할당을 측정한다.
//
// 기대값: ~1 alloc/op (json.NewDecoder 내부 버퍼), ~512 B/op
func BenchmarkCoreXIngest(b *testing.B) {
	svc, cleanup := newBenchInfra()
	defer cleanup()

	handler := infrahttp.NewHTTPHandler(svc)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodPost, "/ingest", bytes.NewBufferString(benchBody))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)
	}
}

// BenchmarkGinIngest는 Gin 핸들러의 메모리 할당을 측정한다.
//
// 기대값: ~2-4 alloc/op, ~1-2 KB/op (Context 풀, 바인딩 오버헤드)
func BenchmarkGinIngest(b *testing.B) {
	gin.SetMode(gin.ReleaseMode)

	svc, cleanup := newBenchInfra()
	defer cleanup()

	r := gin.New()
	r.POST("/ingest", ginIngestHandler(svc))

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodPost, "/ingest", bytes.NewBufferString(benchBody))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()

		r.ServeHTTP(rec, req)
	}
}
