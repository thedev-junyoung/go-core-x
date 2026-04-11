// 이 파일은 package http에 속한다 (handler.go와 동일 패키지).
// 패키지 주석은 handler.go에 위치한다.
package http

import (
	"encoding/json"
	"net/http"

	// application/ingestion.Stats 포트를 통해 엔진 카운터에 접근한다.
	// concrete executor.WorkerPool을 직접 import하지 않는 이유:
	// 이 핸들러가 Stats 포트만 필요로 하며, 포트 인터페이스를 통하면
	// Phase 4에서 Prometheus 구현체로 교체해도 이 파일이 변경되지 않는다.
	appingestion "github.com/junyoung/core-x/internal/application/ingestion"
	infrareplication "github.com/junyoung/core-x/internal/infrastructure/replication"
)

// StatsHandler 는 GET /stats에 대해 엔진 카운터의 JSON 스냅샷을 반환한다.
//
// 왜 Stats를 포트(인터페이스)로 받는가?
// handler.go의 HTTPHandler와 달리, StatsHandler는 Stats 포트를 통해 받는다.
// 이유: Stats와 Submitter는 서로 다른 소비자가 사용하며 (ISP),
// 또한 미래에 Stats 구현체가 executor.WorkerPool 이외의 것으로 교체될 수 있다.
//
// Phase 4 마이그레이션:
// 이 핸들러를 Prometheus /metrics 핸들러로 교체하면 된다.
// Stats 포트와 executor 구현체는 그대로 유지된다 — 이 파일만 변경된다.
//
// 할당 분석:
//   - json.NewEncoder(w): w(http.ResponseWriter)가 io.Writer이므로 중간 버퍼 할당 없음.
//   - map 리터럴: 힙으로 escape됨 (~1 할당). 옵저버빌리티 엔드포인트이므로 허용 가능.
//     핫 경로(POST /ingest)가 아니기 때문에 추가 최적화는 불필요하다.
func StatsHandler(stats appingestion.Stats, lag ...*infrareplication.ReplicationLag) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		type statsResponse struct {
			ProcessedTotal       int64 `json:"processed_total"`
			QueueDepth           int64 `json:"queue_depth"`
			ReplicationLagBytes  int64 `json:"replication_lag_bytes,omitempty"`
			ReplicationReconnects int64 `json:"replication_reconnects,omitempty"`
		}

		resp := statsResponse{
			ProcessedTotal: stats.ProcessedCount(),
			QueueDepth:     int64(stats.QueueDepth()),
		}

		if len(lag) > 0 && lag[0] != nil {
			resp.ReplicationLagBytes = lag[0].Bytes()
			resp.ReplicationReconnects = lag[0].ReconnectCount()
		}

		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.Encode(resp) //nolint:errcheck // best-effort 옵저버빌리티 엔드포인트: 인코딩 에러를 무시해도 운영에 영향 없음
	}
}
