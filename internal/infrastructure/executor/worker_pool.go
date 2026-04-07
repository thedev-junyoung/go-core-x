// Package executor는 비동기 이벤트 처리를 구동하는 고루틴 워커 풀을 구현한다.
//
// 계층 내 위치: Infrastructure Layer.
// 고루틴 생명주기, 배압(backpressure) 채널, 처리 카운터를 소유한다.
//
// 외부 의존성:
//   - 알고 있는 것: domain (Event, EventProcessor), infrastructure/pool (EventPool)
//   - 모르는 것: HTTP, application 계층 — 순수하게 실행 메커니즘만 담당
//
// 구현 인터페이스:
//   - application/ingestion.Submitter (Submit 메서드)
//   - application/ingestion.Stats     (ProcessedCount + QueueDepth 메서드)
//
// 동시성 구조 (Concurrency skeleton):
//   - 단일 버퍼드 jobCh가 유일한 배압 지점이다.
//   - N개의 worker 고루틴이 jobCh를 블로킹으로 소비한다.
//   - 요청당 고루틴을 생성하지 않는다 (goroutine explosion 방지).
//   - processedCount는 atomic.Int64로 업데이트된다 — 핫 경로에서 lock contention 없음.
//
// 확장성 (DDIA Scalability):
//
//	goroutine 수는 생성 시점에 고정된다 (ADR-001).
//	N이 너무 낮으면 → jobCh 포화 → Submit() false → HTTP 429 (명시적 신호).
//	N이 너무 높으면 → 스택 메모리 낭비 (고루틴당 생성 시 ~8 KB).
//	올바른 크기 조정은 부하 테스트가 필요하다; QueueDepth() + ProcessedCount()가 그 데이터를 제공한다.
package executor

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/junyoung/core-x/internal/domain"
	"github.com/junyoung/core-x/internal/infrastructure/pool"
	// 의존성 규칙: executor는 infrastructure/pool을 import해도 된다 (같은 계층).
	// domain과 application 계층은 절대 pool을 직접 import해서는 안 된다.
)

// WorkerPool 은 버퍼드 잡 채널을 드레인하는 고정 크기 고루틴 풀이다.
//
// 필드 레이아웃 설계 의도:
//   - jobCh: 핫 경로의 중심 — 모든 Submit/work 경로가 이 채널을 통과한다.
//   - wg: Shutdown에서만 사용; 핫 경로에서 접근하지 않는다.
//   - processedCount: atomic.Int64 — mutex 없이 N개의 worker가 동시에 증가 가능.
//     false sharing 방지: atomic.Int64는 내부적으로 64-bit 정렬이 보장된다.
//   - processor: worker 고루틴에서만 호출; HTTP 수락 경로에서 접근 없음.
//   - eventPool: worker 고루틴에서만 호출; 처리 후 즉시 Event 반환.
type WorkerPool struct {
	jobCh          chan *domain.Event
	wg             sync.WaitGroup
	processedCount atomic.Int64

	// processor는 이벤트당 한 번 호출되는 domain.EventProcessor다.
	// 필드로 보관하는 이유: Phase 2에서 WAL 라이터로 교체할 때
	// 풀 메커니즘을 전혀 건드리지 않아도 된다.
	// 인터페이스 디스패치 비용 ~2 ns — worker 고루틴 내부이므로 허용 가능.
	processor domain.EventProcessor

	// eventPool은 처리 완료 후 Event를 반환하는 데 사용된다.
	// executor가 infrastructure/pool을 직접 참조하는 이유:
	// 같은 infrastructure 계층이므로 의존성 규칙을 위반하지 않는다.
	// domain/application이 pool을 직접 import하는 것은 허용되지 않는다.
	eventPool *pool.EventPool
}

// Config 는 NewWorkerPool의 매개변수를 그룹화한다.
//
// 왜 Config 구조체인가?
// 매개변수가 4개 이상이면 순서 기반 위치 인자는 실수를 유발한다.
// Config 는 각 매개변수를 명시적으로 명명하고, 미래 확장 시 하위 호환성을 유지한다.
type Config struct {
	// NumWorkers: jobCh를 블로킹으로 소비하는 고루틴 수.
	// 권장값: I/O 바운드는 runtime.NumCPU()*2, CPU 바운드는 runtime.NumCPU().
	// Phase 1(로그 쓰기)은 I/O 바운드에 가깝다.
	NumWorkers int

	// JobBufferSize: 버퍼드 채널의 깊이.
	// 크게 설정하면 버스트를 흡수하지만 caller에게 배압 신호가 늦게 전달된다.
	// 작게 설정하면 과부하 시 더 빠르게 429를 반환한다.
	// 기준값: NumWorkers * 10.
	JobBufferSize int

	// Processor: 각 worker 고루틴이 이벤트당 한 번 호출하는 프로세서.
	// 반드시 goroutine-safe해야 한다 (N개의 worker가 동시에 호출).
	// Process() 반환 후 *Event를 보유(retain)해서는 안 된다 —
	// 풀이 즉시 회수한다. 위반 시 use-after-free.
	Processor domain.EventProcessor

	// EventPool: 처리 완료 후 Event를 반환하는 풀 인스턴스.
	EventPool *pool.EventPool
}

// NewWorkerPool 은 워커 풀을 생성하고 즉시 worker 고루틴들을 시작한다.
//
// worker들은 생성 즉시 jobCh 드레인을 시작한다.
// 정상 종료는 Shutdown으로 수행한다.
//
// 사전 조건: cfg.Processor와 cfg.EventPool은 nil이 아니어야 한다.
func NewWorkerPool(cfg Config) *WorkerPool {
	wp := &WorkerPool{
		jobCh:     make(chan *domain.Event, cfg.JobBufferSize), // 단일 배압 지점
		processor: cfg.Processor,
		eventPool: cfg.EventPool,
	}

	for range cfg.NumWorkers {
		wp.wg.Add(1)
		go wp.work()
	}

	slog.Info("worker pool started",
		"workers", cfg.NumWorkers,
		"buffer_depth", cfg.JobBufferSize,
	)
	return wp
}

// Submit 은 Event를 비동기 처리를 위해 enqueue하려 시도한다.
// jobCh가 가득 찼으면 false를 즉시 반환한다 (non-blocking select/default).
//
// 왜 non-blocking인가? (ADR-001)
// 블로킹 전송은 과부하 상황에서 HTTP handler 고루틴을 점유시킨다.
// 블로킹이 누적되면 고루틴 수가 폭발적으로 증가해 메모리 압력이 가중된다.
// 반면 false → 429 반환은 CPU 비용이 미미하며 caller에게 명시적이고 즉각적인 신호를 준다.
//
// "좋은 거절"의 의미 (DDIA 신뢰성):
// 무한 대기나 silent drop 대신 명시적 429는 클라이언트가 백오프·재시도할 수 있게 한다.
// 시스템이 과부하를 스스로 인식하고 상류에 알리는 것이 신뢰성의 핵심이다.
func (wp *WorkerPool) Submit(e *domain.Event) bool {
	select {
	case wp.jobCh <- e: // 채널에 공간이 있으면 즉시 enqueue
		return true
	default: // 채널이 가득 찼으면 즉시 거절 — 블로킹하지 않는다
		return false
	}
}

// ProcessedCount 는 시작 이후 처리된 이벤트 총수를 반환한다.
//
// atomic.Load 사용: 어느 고루틴에서 호출해도 안전하며 lock contention이 없다.
// stats 엔드포인트와 Phase 4 Prometheus 메트릭이 이 값을 사용한다.
func (wp *WorkerPool) ProcessedCount() int64 {
	return wp.processedCount.Load()
}

// QueueDepth 는 jobCh에서 현재 대기 중인 이벤트 수를 반환한다.
//
// len(ch)는 snapshot 값이므로 이 호출 이후 즉시 변할 수 있다.
// 정확한 현재 상태보다는 추세 모니터링 용도로 사용해야 한다.
// QueueDepth 가 JobBufferSize에 가까우면 429 비율이 증가하고 있다는 신호다.
func (wp *WorkerPool) QueueDepth() int {
	return len(wp.jobCh)
}

// Shutdown 은 jobCh를 닫아 worker들에게 드레인 후 종료하도록 신호를 보내고,
// 모든 worker가 완료될 때까지 기다린다. ctx는 최대 대기 시간을 제어한다.
//
// Drain-before-stop 불변식 (DDIA 신뢰성):
// close(jobCh) → for range 루프가 남은 항목을 모두 소비한 뒤 종료.
// 이 불변식이 보장하는 것: 정상 종료 시 큐에 있던 이벤트가 silent drop되지 않음.
//
// 호출 순서 제약 (cmd/main.go가 반드시 지켜야 하는 계약):
// Shutdown 은 HTTP 서버가 새 요청 수락을 중단한 뒤에만 호출해야 한다.
// 순서를 뒤집으면 in-flight Submit()이 닫힌 채널로 전송을 시도해 패닉이 발생한다.
// cmd/main.go는 server.Shutdown() → workerPool.Shutdown() 순서를 보장한다.
func (wp *WorkerPool) Shutdown(ctx context.Context) {
	close(wp.jobCh) // worker들에게 드레인 후 종료 신호

	done := make(chan struct{})
	go func() {
		wp.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		slog.Info("worker pool shutdown complete",
			"total_processed", wp.processedCount.Load(),
		)
	case <-ctx.Done():
		slog.Warn("worker pool shutdown timed out; some events may be unprocessed",
			"total_processed", wp.processedCount.Load(),
		)
	}
}

// work 는 단일 worker 고루틴의 본체다.
// jobCh가 닫히고 완전히 드레인될 때까지 루프를 돈다.
//
// for range 채널의 드레인 보증:
// close(jobCh) 이후에도 버퍼에 남은 항목이 있으면 모두 소비한다.
// 빈 채널이 닫히면 루프가 종료된다 — 명시적 신호 채널 없이 자연스럽게 종료된다.
//
// 처리 순서: Process → 카운터 증가 → Release.
// Release 가 Process 이후에 호출되는 이유:
// Processor 가 e를 사용하는 동안 풀이 회수하면 use-after-free가 발생한다.
// Config.Processor 계약(반환 후 retain 금지)에 따라 Process() 반환 직후 Release가 안전하다.
func (wp *WorkerPool) work() {
	defer wp.wg.Done()

	for e := range wp.jobCh {
		wp.processor.Process(e)
		wp.processedCount.Add(1) // atomic 증가: lock 없이 N개 worker 동시 안전
		wp.eventPool.Release(e)  // Process() 반환 후 즉시 반환: retain 금지 계약 이행
	}
}
