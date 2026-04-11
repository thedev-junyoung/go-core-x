// Package ingestion은 수집 유스케이스의 애플리케이션 계층이다.
//
// 계층 내 위치: Application Layer (육각형 아키텍처의 내부 육각형).
// 트랜스포트(HTTP, gRPC)와 인프라(sync.Pool, 채널)의 구체적 구현을 알지 못한다.
//
// 책임:
//   - Ingest 유스케이스를 오케스트레이션한다: 풀에서 Event 획득 → 채우기 → 비동기 제출.
//   - 비즈니스 규칙(어떤 이벤트를 거절할 것인가)을 소유한다.
//
// 외부 의존성:
//   - 알고 있는 것: domain 패키지 (Event, EventProcessor 타입)
//   - 모르는 것: sync.Pool, 채널, HTTP, goroutine — 이 패키지는 그것들을 직접 다루지 않는다.
//
// 포트 패턴 (ADR-001):
// 이 파일에 정의된 인터페이스들은 의존성 역전 경계다.
// 구체적 구현은 infrastructure/ 에 위치하며, 이 패키지는 절대 그것을 import하지 않는다.
// 덕분에 HTTP 없이도 유스케이스를 단위 테스트할 수 있다.
//
// 인터페이스 디스패치 비용: 요청당 최대 3회 (Submitter.Submit + EventPool.Acquire + 거절 시 EventPool.Release).
// 총 ~6 ns — 네트워크 지연(수십 µs)에 비하면 무시 가능한 수준이다.
package ingestion

import (
	"errors"
	"fmt"
	"time"

	"github.com/junyoung/core-x/internal/domain"
)

// ErrOverloaded 는 하위 Submitter가 포화 상태일 때 Ingest가 반환하는 sentinel 에러다.
//
// 왜 fmt.Errorf 래퍼가 아닌 sentinel인가?
// fmt.Errorf("...%w", err)는 매 호출마다 힙 할당을 발생시킨다.
// 과부하 상황에서는 초당 수천 번 이 경로가 호출될 수 있으므로,
// 사전 할당된 sentinel을 반환하면 에러 경로의 할당이 0이 된다.
// errors.Is() 비교와 완전히 호환된다.
var ErrOverloaded = errors.New("ingestion: worker pool saturated")

// Submitter 는 비동기 이벤트 디스패치를 추상화하는 포트다.
//
// 구현체 (executor.WorkerPool): non-blocking 채널 전송을 사용한다.
// false 반환은 채널이 가득 찼음(포화)을 의미하며,
// 서비스는 이를 ErrOverloaded로 변환해 caller에게 명시적 신호를 보낸다.
//
// 왜 bool 반환인가 (error 대신)?
// 성공/실패 두 가지 상태만 존재하는 경로에서 bool이 더 명확하다.
// 에러 객체 생성 비용도 제거된다.
type Submitter interface {
	Submit(e *domain.Event) bool
}

// Stats 는 엔진 카운터를 옵저버빌리티에 노출하는 포트다.
//
// 왜 별도 인터페이스로 분리했는가?
// Submitter(쓰기 경로)와 Stats(읽기 경로)는 서로 다른 소비자가 사용한다.
// HTTP handler는 Submitter만 필요하고, stats endpoint는 Stats만 필요하다.
// 인터페이스를 합치면 테스트 더블이 불필요하게 커지고,
// 인터페이스 분리 원칙(ISP)을 위반한다.
//
// Phase 4 마이그레이션: Prometheus /metrics 엔드포인트로 교체 시,
// infrastructure/http/stats.go만 변경된다.
// executor.WorkerPool과 이 포트는 그대로 유지된다.
type Stats interface {
	ProcessedCount() int64
	QueueDepth() int
}

// EventPool 은 풀링된 Event 할당을 추상화하는 포트다.
//
// 왜 sync.Pool을 직접 참조하지 않는가?
// infrastructure/pool.EventPool이 sync.Pool 기반이지만,
// 이 계층은 그 사실을 알면 안 된다. 테스트 시 non-pooled 더블을 주입하고,
// 미래에 arena allocator나 slab allocator로 교체할 수 있어야 한다.
// 인터페이스 분리가 그 유연성을 보장한다.
type EventPool interface {
	Acquire() *domain.Event
	Release(e *domain.Event)
}

// WALWriter 는 이벤트를 WAL(Write-Ahead Log)에 기록하는 포트다.
//
// 구현체 (storage/wal.Writer): 파일 기반 순차 기록.
// Phase 2에서 도입되며, Submitter 전에 호출된다.
//
// 왜 WriteEvent(e *domain.Event) 인가?
// application 계층은 storage/wal을 직접 import하지 않는다 (의존성 역전).
// Event를 바이너리로 encode하는 로직은 WALWriter 구현체(storage/wal)가 담당한다.
// application은 단순히 "Event를 로그에 기록하라"는 의도만 표현한다.
type WALWriter interface {
	WriteEvent(e *domain.Event) error
}

// IngestionService 는 Ingest 유스케이스를 소유하는 애플리케이션 계층 객체다.
//
// 필드:
//   - submitter: 비동기 처리를 위해 이벤트를 WorkerPool로 전송.
//   - pool: 이벤트 객체 재사용을 위한 메모리 풀.
//   - walWriter: 이벤트를 WAL 파일에 기록 (Phase 2). 선택적 (nil 가능).
//
// 왜 포트를 인터페이스 필드로 보유하는가?
// 각 포트(submitter, pool, walWriter)는 테스트 시 더블로 교체 가능해야 한다.
// 이 유연성이 애플리케이션 계층의 핵심 가치다.
//
// 왜 walWriter는 nil 가능한가?
// Phase 1(로그 출력만)과 Phase 2+(WAL 기록)를 모두 지원하기 위해서다.
// cmd/main.go는 필요에 따라 WAL Writer를 주입하거나 nil을 전달한다.
type IngestionService struct {
	submitter Submitter
	pool      EventPool
	walWriter WALWriter
	metrics   IngestMetrics
}

// NewIngestionService 는 필수 포트와 선택적 포트를 주입받아 서비스를 생성한다.
//
// 사전 조건:
//   - submitter와 pool은 nil이 아니어야 한다.
//   - walWriter는 nil 가능 (WAL 기록이 필요 없으면 nil 전달).
//
// nil 주입 시:
//   - submitter/pool이 nil: 첫 번째 Ingest 호출에서 패닉 발생 (의도된 동작).
//   - walWriter가 nil: WAL 기록 단계를 skip (정상 동작).
func NewIngestionService(submitter Submitter, pool EventPool, walWriter WALWriter) *IngestionService {
	return &IngestionService{
		submitter: submitter,
		pool:      pool,
		walWriter: walWriter,
	}
}

// SetMetrics attaches an IngestMetrics implementation.
// Call once during wiring before first Ingest().
func (s *IngestionService) SetMetrics(m IngestMetrics) {
	s.metrics = m
}

// Ingest 는 핵심 유스케이스다: 트랜스포트 계층에서 원시 이벤트를 받아
// 타임스탬프를 찍고, WAL에 기록한 후(있으면), 비동기 처리를 위해 디스패치한다.
//
// 절차:
//   1. 이벤트 객체를 풀에서 획득.
//   2. 타임스탬프 + Source/Payload 설정.
//   3. WAL 기록 (walWriter != nil일 때만).
//   4. Submitter에 enqueue 시도.
//   5. 거절 시 풀에 반환.
//
// 핫 경로 할당 분석 (ADR-003, WAL 추가 후):
//   - pool.Acquire(): 풀 히트 시 0 할당 (sync.Pool 빠른 경로)
//   - time.Now(): 스택 할당
//   - walWriter.Write(): payload 직렬화 (~small bytes로 제한) — O(1) 할당
//   - submitter.Submit(): non-blocking 채널 전송 — 0 할당
//   - 거절 경로: pool.Release() + ErrOverloaded sentinel — 0 할당
//
// 순 결과: WAL 미사용 시 기존과 동일 (0 할당). WAL 사용 시 직렬화 버퍼 할당 1회.
//
// 에러 처리:
//   - WAL 쓰기 실패: ErrWALWriteFailed 반환, Event 풀에 반환, submitter에 전달 안 함.
//   - Submitter 포화: ErrOverloaded 반환, Event 풀에 반환.
//   - 성공: nil 반환, Event는 Submitter → Worker → EventPool.Release 경로로 흐름.
//
// 사후 조건 (postcondition):
//   - nil 반환: Event가 jobCh에 성공적으로 enqueue되었음. 처리는 비동기.
//   - error 반환: Event가 풀로 반환되었음. 누출 없음. 에러 종류에 따라 재시도 또는 429 반환.
func (s *IngestionService) Ingest(source, payload string) error {
	var start time.Time
	if s.metrics != nil {
		start = time.Now()
	}

	e := s.pool.Acquire()
	e.ReceivedAt = time.Now()
	e.Source = source
	e.Payload = payload

	// WAL 기록 (walWriter가 제공되었으면).
	if s.walWriter != nil {
		if err := s.walWriter.WriteEvent(e); err != nil {
			s.pool.Release(e)
			if s.metrics != nil {
				s.metrics.RecordIngest("wal_error", time.Since(start).Seconds())
			}
			return fmt.Errorf("wal write failed: %w", err)
		}
	}

	// Submitter에 enqueue 시도.
	if !s.submitter.Submit(e) {
		s.pool.Release(e) // 풀 객체를 절대 누출하지 않는다: 거절 즉시 반환
		if s.metrics != nil {
			s.metrics.RecordIngest("overloaded", time.Since(start).Seconds())
		}
		return ErrOverloaded
	}

	if s.metrics != nil {
		s.metrics.RecordIngest("ok", time.Since(start).Seconds())
	}
	return nil
}

// ReleaseEvent 는 처리된 Event를 풀에 반환하는 헬퍼다.
//
// 왜 이 메서드가 존재하는가?
// executor.WorkerPool은 infrastructure/pool을 직접 import해도 되지만
// (같은 infrastructure 계층이므로 허용),
// 이 서비스를 통해 반환하는 테스트나 caller를 위한 편의 진입점이다.
// pool 패키지를 모르는 caller도 서비스를 통해 Event를 안전하게 반환할 수 있다.
func (s *IngestionService) ReleaseEvent(e *domain.Event) {
	s.pool.Release(e)
}
