// Package http는 수집 엔진의 HTTP 트랜스포트 계층을 구현한다.
//
// 계층 내 위치: Infrastructure Layer (트랜스포트 어댑터).
// 비즈니스 로직은 없다 — 오직 프로토콜 관심사만 다룬다 (파싱, 상태 코드, 헤더).
//
// 책임:
//   - HTTP 와이어 포맷 ↔ Raft Propose 경로 사이의 변환 (Phase 12b: unified path only)
//   - 프로토콜 에러를 HTTP 상태 코드로 매핑
//   - 요청 유효성 검사 (transport-level만: 구조, 필수 필드)
//
// 외부 의존성:
//   - 알고 있는 것: infrastructure/raft.RaftGroupRegistry, KVStateMachine
//   - 모르는 것: application 비즈니스 로직 — HTTP 핸들러는 그것을 몰라도 된다.
//
// Phase 12b (ADR-021 Step 4): legacy Bitcask direct-write path removed.
// HTTPHandler always routes through Raft Propose → WaitForIndex.
// registry and kvSM must be non-nil; NewHTTPHandler panics otherwise.
package http

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/junyoung/core-x/internal/infrastructure/cluster"
	infragrpc "github.com/junyoung/core-x/internal/infrastructure/grpc"
	infraraft "github.com/junyoung/core-x/internal/infrastructure/raft"
)

// ingestRequest는 POST /ingest의 JSON 와이어 포맷이다.
//
// 왜 domain.Event가 아닌 별도 타입인가?
// 와이어 포맷과 내부 표현을 분리한다.
// HTTP 클라이언트가 JSON 스키마에 의존하고, 내부 Event가 변경돼도
// 이 타입을 통해 변환하면 하위 호환성을 유지할 수 있다.
type ingestRequest struct {
	Source  string `json:"source"`
	Payload string `json:"payload"`
}

// HTTPHandler 는 POST /ingest 요청을 처리한다.
//
// Phase 12b (ADR-021): storageUnified 분기가 제거됐다. 모든 로컬 쓰기 경로는
// Raft Propose → WaitForIndex를 사용한다 (linearizable write).
//
// 클러스터 모드에서는 consistent hashing으로 담당 노드를 결정하고,
// 자신이 아니면 gRPC forwarding한다 (ring 로직은 Phase 3 이래 변경 없음).
//
// http.Handler를 구현하므로 실제 서버를 띄우지 않고
// httptest.NewRecorder로 테스트할 수 있다.
type HTTPHandler struct {
	ring           *cluster.Ring        // nil이면 단일 노드 모드
	selfID         string               // 자신의 노드 ID
	forwarder      *infragrpc.Forwarder // gRPC 포워더
	forwardTimeout time.Duration        // 포워딩 RPC 타임아웃

	// Unified write path fields. Always non-nil (enforced by NewHTTPHandler).
	registry *infraraft.RaftGroupRegistry
	kvSM     *infraraft.KVStateMachine
}

// NewHTTPHandler 는 unified write path 핸들러를 생성한다 (Phase 12b 단일 생성자).
//
// registry와 kvSM은 nil이어서는 안 된다. nil이면 panic한다.
// ring이 nil이면 단일 노드 모드로 동작한다 (gRPC forwarding 없음).
func NewHTTPHandler(
	ring *cluster.Ring,
	selfID string,
	forwarder *infragrpc.Forwarder,
	forwardTimeout time.Duration,
	registry *infraraft.RaftGroupRegistry,
	kvSM *infraraft.KVStateMachine,
) *HTTPHandler {
	if registry == nil {
		panic("http: NewHTTPHandler requires non-nil registry")
	}
	if kvSM == nil {
		panic("http: NewHTTPHandler requires non-nil kvSM")
	}
	if forwardTimeout <= 0 {
		forwardTimeout = 3 * time.Second
	}
	return &HTTPHandler{
		ring:           ring,
		selfID:         selfID,
		forwarder:      forwarder,
		forwardTimeout: forwardTimeout,
		registry:       registry,
		kvSM:           kvSM,
	}
}

// NewUnifiedHTTPHandler is an alias for NewHTTPHandler retained for test
// compatibility. Callers should migrate to NewHTTPHandler.
//
// Deprecated: use NewHTTPHandler directly.
func NewUnifiedHTTPHandler(
	_ interface{}, // svc — no longer used; accepted for call-site compatibility
	ring *cluster.Ring,
	selfID string,
	forwarder *infragrpc.Forwarder,
	forwardTimeout time.Duration,
	registry *infraraft.RaftGroupRegistry,
	kvSM *infraraft.KVStateMachine,
) *HTTPHandler {
	return NewHTTPHandler(ring, selfID, forwarder, forwardTimeout, registry, kvSM)
}

// ServeHTTP 는 POST /ingest를 처리한다.
//
// 핫 경로 할당 분석 (ADR-003):
//   - json.NewDecoder: ~1 할당 (512 byte 내부 읽기 버퍼). stdlib의 불가피한 비용.
//   - var req ingestRequest: 스택 할당. 컴파일러 escape analysis가 포인터를 저장하지 않는 한 스택에 유지.
//   - serveUnifiedIngest: json.Marshal(RaftKVCommand) 1 할당 — Raft propose 경로의 필수 비용.
//
// 순 결과: 수락된 요청당 ~2 할당.
//
// 에러 매핑 원칙:
//   - 파싱 실패 → 400 Bad Request (클라이언트 오류)
//   - 필수 필드 누락 → 422 Unprocessable Entity (의미론적 유효성 실패)
//   - Raft not leader → 503 Service Unavailable
//   - WaitForIndex timeout → 504 Gateway Timeout
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
	// ring이 nil이면 단일 노드 모드 — 로컬 처리 경로로 직행한다.
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

	// Phase 12b: all local writes go through Raft Propose → WaitForIndex.
	// Linearizable write: 204 is returned only after the entry is committed and applied.
	h.serveUnifiedIngest(w, r, req)
}

// unifiedIngestTimeout is the maximum time serveUnifiedIngest waits for Raft
// to apply the proposed entry. Bounded so goroutines do not leak when the
// cluster is partitioned.
const unifiedIngestTimeout = 5 * time.Second

// serveUnifiedIngest handles the local write path via Raft consensus.
//
// Flow:
//  1. registry.Get(partitionID) — look up the owning RaftNode
//  2. RaftNode.Propose(marshalledCmd) — propose to the Raft cluster
//  3. If !isLeader → 503 Service Unavailable
//  4. KVStateMachine.WaitForIndex(ctx, index) — linearizable write guarantee
//  5. 204 No Content on success (write is committed and applied)
//
// Invariants:
//   - registry and kvSM are always non-nil (enforced by NewHTTPHandler).
func (h *HTTPHandler) serveUnifiedIngest(w http.ResponseWriter, r *http.Request, req ingestRequest) {
	partitionID := infraraft.PartitionID(h.selfID)
	node, ok := h.registry.Get(partitionID)
	if !ok {
		slog.Error("unified ingest: no raft node for partition", "partition", partitionID)
		http.Error(w, "raft partition unavailable", http.StatusServiceUnavailable)
		return
	}

	cmd := infraraft.RaftKVCommand{Op: "set", Key: req.Source, Value: req.Payload}
	data, err := json.Marshal(cmd)
	if err != nil {
		// json.Marshal on a simple struct should never fail.
		slog.Error("unified ingest: marshal command failed", "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	index, _, isLeader := node.Propose(data)
	if !isLeader {
		slog.Warn("unified ingest: not the Raft leader", "partition", partitionID)
		http.Error(w, "not the Raft leader — retry on the leader node", http.StatusServiceUnavailable)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), unifiedIngestTimeout)
	defer cancel()

	if err := h.kvSM.WaitForIndex(ctx, index); err != nil {
		slog.Warn("unified ingest: timed out waiting for Raft apply",
			"index", index, "err", err)
		http.Error(w, "timed out waiting for Raft apply", http.StatusGatewayTimeout)
		return
	}

	w.WriteHeader(http.StatusNoContent) // 204: committed and applied
}
