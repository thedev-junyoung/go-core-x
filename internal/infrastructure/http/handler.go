// Package http는 수집 엔진의 HTTP 트랜스포트 계층을 구현한다.
//
// 계층 내 위치: Infrastructure Layer (트랜스포트 어댑터).
// 비즈니스 로직은 없다 — 오직 프로토콜 관심사만 다룬다 (파싱, 상태 코드, 헤더).
// 모든 도메인 결정은 application 계층의 IngestionService가 소유한다.
//
// 책임:
//   - HTTP 와이어 포맷 ↔ 애플리케이션 유스케이스 사이의 변환
//   - 프로토콜 에러를 HTTP 상태 코드로 매핑
//   - 요청 유효성 검사 (transport-level만: 구조, 필수 필드)
//
// 외부 의존성:
//   - 알고 있는 것: application/ingestion.IngestionService (concrete 타입, 아래 이유 참조)
//   - 모르는 것: sync.Pool, 채널, domain 내부 — HTTP 핸들러는 그것을 몰라도 된다.
//
// 왜 handler.go가 *IngestionService를 인터페이스가 아닌 concrete 타입으로 보유하는가? (ADR-002)
// HTTP handler는 정확히 하나의 유스케이스에 결합되어 있다.
// interface dispatch는 itab 조회 ~2 ns의 비용이 있지만 실질적인 유연성(교체 가능성)이 없다.
// concrete 참조 = 핫 경로에서 디스패치 비용 제거.
// 테스트에서 교체가 필요할 경우: httptest.NewRecorder로 핸들러를 직접 생성하면 된다.
package http

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	// application 계층을 import한다: 허용된 방향 (infrastructure → application).
	// application이 infrastructure를 import하는 역방향은 의존성 규칙 위반이다.
	appingestion "github.com/junyoung/core-x/internal/application/ingestion"
	"github.com/junyoung/core-x/internal/infrastructure/cluster"
	infragrpc "github.com/junyoung/core-x/internal/infrastructure/grpc"
)

// ingestRequest는 POST /ingest의 JSON 와이어 포맷이다.
//
// 왜 domain.Event가 아닌 별도 타입인가?
// 와이어 포맷과 내부 표현을 분리한다.
// HTTP 클라이언트가 JSON 스키마에 의존하고, 내부 Event가 변경돼도
// 이 타입을 통해 변환하면 하위 호환성을 유지할 수 있다.
//
// Phase 3 마이그레이션 경로: protobuf로 교체 시 이 타입을 protobuf 언마샬로 대체한다.
// domain.Event와 IngestionService는 변경되지 않는다.
type ingestRequest struct {
	Source  string `json:"source"`
	Payload string `json:"payload"`
}

// HTTPHandler 는 POST /ingest 요청을 처리한다.
//
// Phase 3 확장: ring과 forwarder가 주입되면 consistent hashing으로
// 담당 노드를 결정하고 필요 시 gRPC로 forwarding한다.
// ring이 nil이거나 selfID가 담당 노드이면 기존 로컬 처리 경로를 그대로 사용한다.
//
// http.Handler를 구현하므로 실제 서버를 띄우지 않고
// httptest.NewRecorder로 테스트할 수 있다.
type HTTPHandler struct {
	svc            *appingestion.IngestionService // concrete 참조: 핫 경로 itab 조회 제거 (ADR-002)
	ring           *cluster.Ring                 // nil이면 단일 노드 모드
	selfID         string                        // 자신의 노드 ID
	forwarder      *infragrpc.Forwarder          // gRPC 포워더
	forwardTimeout time.Duration                 // 포워딩 RPC 타임아웃
}

// NewHTTPHandler 는 핸들러를 수집 서비스에 연결한다 (단일 노드 모드).
func NewHTTPHandler(svc *appingestion.IngestionService) *HTTPHandler {
	return &HTTPHandler{svc: svc}
}

// NewClusterHTTPHandler 는 클러스터 모드 핸들러를 생성한다.
// ring.Lookup으로 담당 노드를 결정하고, 자신이 아니면 forwarder로 gRPC forward한다.
func NewClusterHTTPHandler(
	svc *appingestion.IngestionService,
	ring *cluster.Ring,
	selfID string,
	forwarder *infragrpc.Forwarder,
	forwardTimeout time.Duration,
) *HTTPHandler {
	if forwardTimeout <= 0 {
		forwardTimeout = 3 * time.Second
	}
	return &HTTPHandler{
		svc:            svc,
		ring:           ring,
		selfID:         selfID,
		forwarder:      forwarder,
		forwardTimeout: forwardTimeout,
	}
}

// ServeHTTP 는 POST /ingest를 처리한다.
//
// 핫 경로 할당 분석 (ADR-003):
//   - json.NewDecoder: ~1 할당 (512 byte 내부 읽기 버퍼). stdlib의 불가피한 비용.
//     프로파일링에서 핫스팟으로 표시되면 bytedance/sonic 또는 풀링된 디코더를 평가하라.
//     측정 전에 풀링하지 말 것 — 복잡성 증가 대비 성과가 보장되지 않는다.
//   - var req ingestRequest: 스택 할당. 컴파일러 escape analysis가 포인터를 저장하지 않는 한 스택에 유지.
//   - svc.Ingest(): happy path에서 0 할당 (풀 히트 + 제출 성공).
//
// 순 결과: 수락된 요청당 ~1 할당.
//
// 에러 매핑 원칙:
//   - 파싱 실패 → 400 Bad Request (클라이언트 오류)
//   - 필수 필드 누락 → 422 Unprocessable Entity (의미론적 유효성 실패)
//   - ErrOverloaded → 429 Too Many Requests ("좋은 거절" — 클라이언트에게 백오프 신호)
//   - 기타 → 500 Internal Server Error (Phase 1에서는 발생 불가)
func (h *HTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req ingestRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields() // 스키마 위반 시 즉시 실패: 디버깅 편의성 + 클라이언트 계약 강제
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.Source == "" || req.Payload == "" {
		http.Error(w, "source and payload are required", http.StatusUnprocessableEntity)
		return
	}

	// Phase 3: 클러스터 모드이면 consistent hashing으로 담당 노드를 결정한다.
	// ring이 nil이면 단일 노드 모드 — 기존 로컬 처리 경로로 직행한다.
	if h.ring != nil {
		if target, ok := h.ring.Lookup(req.Source); ok && target.ID != h.selfID {
			if !target.IsHealthy() {
				slog.Warn("target node unhealthy; rejecting request",
					"source", req.Source, "target_node", target.ID)
				http.Error(w, "target node unavailable", http.StatusServiceUnavailable)
				return
			}
			// gRPC forward: 담당 노드에 요청을 전달한다.
			ctx, cancel := context.WithTimeout(r.Context(), h.forwardTimeout)
			defer cancel()
			if err := h.forwarder.Forward(ctx, target, req.Source, []byte(req.Payload)); err != nil {
				slog.Error("forward failed", "source", req.Source, "target", target.ID, "err", err)
				http.Error(w, "target node unavailable", http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusAccepted)
			return
		}
	}

	// 로컬 처리 경로 (단일 노드 모드 또는 자신이 담당 노드인 경우).
	if err := h.svc.Ingest(req.Source, req.Payload); err != nil {
		if errors.Is(err, appingestion.ErrOverloaded) {
			// 429는 "좋은 거절"이다: silent drop이나 무한 대기 대신
			// 클라이언트에게 명시적으로 백오프를 요청한다. DDIA 신뢰성 원칙.
			slog.Warn("worker pool saturated; shedding request", "source", req.Source)
			http.Error(w, "server overloaded, retry later", http.StatusTooManyRequests)
			return
		}
		// 예상치 못한 에러 경로.
		// Phase 4에서 Prometheus 카운터로 계측한다.
		slog.Error("ingest error", "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusAccepted) // 202: 비동기 처리를 위해 수락됨, 아직 커밋되지 않음
}
