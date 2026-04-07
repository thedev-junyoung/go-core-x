# ADR-003: Clean Architecture 계층 분리 — Zero-Allocation 성능 보존 전략

- **Status**: Accepted
- **Date**: 2026-04-07
- **Deciders**: Core-X Principal Architect

---

## Context

### Phase 1 목표와 도전 과제

Phase 1의 핵심 목표는 **초당 수만 건의 JSON 이벤트를 무손실로 수집**하는 것이다. 이 목표는 두 가지 상충하는 요구를 동시에 만족해야 한다는 점에서 도전적이다.

**성능 요구사항:**
- Hot path allocation: 요청당 1개 이하 (JSON 디코딩은 stdlib 한계, 그 외 zero-alloc)
- GC pause: 최소화 (pooled allocation으로 GC trigger 빈도 억제)
- p99 latency: HTTP accept → worker dispatch 경로에서 ~500 ns 이내
- Backpressure: 워커풀 포화 시 즉각 HTTP 429 반환, goroutine 누적 없음

**유지보수성 요구사항 (DDIA: Maintainability):**
- Phase 2에서 WAL persistence를 추가할 때 HTTP handler가 변경되어서는 안 된다
- Phase 3에서 gRPC 수집 엔드포인트를 추가할 때 비즈니스 규칙(validation)을 재작성하지 않아야 한다
- Phase 4에서 Prometheus 메트릭으로 전환할 때 도메인 로직이 영향받지 않아야 한다

**핵심 긴장**: 전통적인 Clean Architecture는 모든 경계를 인터페이스로 추상화한다. 인터페이스마다 itab 조회 비용 ~2 ns가 추가되고, 추상화 레이어마다 테스트 더블이 필요해 코드 복잡도가 증가한다. 그러나 추상화를 전혀 하지 않으면 Phase 2~4의 변경이 코드 전체에 파급된다.

**왜 Clean Architecture인가?**

"기술 부채 없는 확장 기반"을 만들기 위해 Clean Architecture를 선택했다. 여기서 "Clean"은 폴더 구조의 미학이 아니라 **변경이 전파되는 범위를 제한하는 능력**을 의미한다.

Clean Architecture의 핵심 가치는 Dependency Rule이다: 의존 방향은 항상 바깥 레이어에서 안쪽 레이어로만 흘러야 한다. 이 규칙이 지켜지면, 안쪽 레이어(Domain, Application)는 바깥 레이어(Infrastructure)의 변경에 완전히 무관하다.

```
Infrastructure  →  Application  →  Domain
Infrastructure  →  Domain
(반대 방향 의존 금지)
```

---

## Decision

### 1. 계층 분리 전략

#### Domain Layer (`internal/domain/`)

**무엇을 안다**: 핵심 비즈니스 엔티티와 계약
**무엇을 모른다**: HTTP, goroutine, sync.Pool, 채널

```go
// domain/event.go
// stdlib time 외 import 없음. 이 레이어가 변하면 모든 레이어에 파급된다.
// → 안정적으로 유지하는 것이 최우선.

type Event struct {
    ReceivedAt time.Time  // 24 bytes
    Source     string     // 16 bytes
    Payload    string     // 16 bytes
}
// 총 56 bytes, 필드 정렬 최적화. 수만 개가 채널 버퍼에 동시 존재할 때
// 잘못된 패딩은 숨겨진 메모리 오버헤드가 된다.

type EventProcessor interface {
    Process(e *Event)
}

type EventProcessorFunc func(e *Event)
func (f EventProcessorFunc) Process(e *Event) { f(e) }
```

`EventProcessorFunc`는 `http.HandlerFunc` 패턴을 따른다. 이름 있는 struct를 정의하지 않고도 함수 리터럴이 인터페이스를 만족하게 해 `cmd/main.go`의 wiring을 간결하게 유지한다.

#### Application Layer (`internal/application/ingestion/`)

**무엇을 안다**: 도메인 타입, 포트 인터페이스 (자신이 정의)
**무엇을 모른다**: 채널, HTTP 상태 코드, sync.Pool 구현, goroutine

Port 패턴으로 의존성을 역전한다. Application이 Infrastructure를 알지 않고, Infrastructure가 Application의 포트를 구현한다.

```go
// application/ingestion/service.go

// 3개의 포트를 정의. 구현체는 모두 infrastructure 레이어에 있다.
type Submitter interface {
    Submit(e *domain.Event) bool
}
type Stats interface {
    ProcessedCount() int64
    QueueDepth() int
}
type EventPool interface {
    Acquire() *domain.Event
    Release(e *domain.Event)
}
```

#### Infrastructure Layer (`internal/infrastructure/`)

**무엇을 안다**: 도메인 타입, Application 포트, HTTP/채널/sync.Pool 등 구체 기술
**무엇을 모른다**: 다른 Infrastructure 패키지 (같은 레이어 내 직접 import는 예외, 아래 참조)

#### Composition Root (`cmd/main.go`)

**무엇을 안다**: 모든 concrete type — 의도적으로.
이 파일은 Dependency Rule의 예외 지점이다. 모든 레이어를 알고 조립하는 것이 Composition Root의 본질이다. 여기서의 import 방향 위반은 아키텍처 결함이 아니라 설계된 wiring 지점이다.

```go
// cmd/main.go — 의존성 그래프:
// pool.EventPool ──────────────────────────────┐
// executor.WorkerPool (Submitter + Stats 구현) ─┤
//                                               ▼
//                        application/ingestion.IngestionService
//                                               │
//                        infrastructure/http.HTTPHandler
```

### 2. Sentinel Error 패턴

`ErrOverloaded`를 `errors.New()`로 미리 할당한다.

```go
// application/ingestion/service.go
var ErrOverloaded = errors.New("ingestion: worker pool saturated")
```

**왜 sentinel인가?**

`fmt.Errorf("saturated: queue depth %d", depth)` 같은 동적 에러 생성은 rejection path마다 heap allocation을 유발한다. 워커풀이 포화된 상황은 이미 과부하 상태다. 이 경로에서 GC를 더 자극하는 것은 최악의 타이밍에 latency spike를 추가한다.

Sentinel error는 프로세스 시작 시 1회 할당, 이후 포인터 비교만 발생한다. `errors.Is()` 사용으로 인터페이스 계약은 그대로 유지된다. 에러 메시지에 동적 정보가 필요하다면 `errors.Is()` 분기 후 별도 로깅으로 분리한다 (`handler.go`에서 실제로 이렇게 구현됨).

```go
// infrastructure/http/handler.go
if errors.Is(err, appingestion.ErrOverloaded) {
    slog.Warn("worker pool saturated; shedding request", "source", req.Source)
    http.Error(w, "server overloaded, retry later", http.StatusTooManyRequests)
    return
}
```

### 3. Concrete vs. Interface 타입 선택 기준

"모든 경계는 인터페이스여야 한다"는 규칙을 따르지 않는다. 교체 가능성이 실질적으로 존재하는 경계에만 인터페이스를 도입한다.

**인터페이스로 감싼 타입들:**

| 타입 | 위치 | 이유 |
|------|------|------|
| `Submitter` | Application 포트 | Phase 2에서 WAL-backed submitter로 교체 가능해야 함 |
| `Stats` | Application 포트 | Phase 4에서 Prometheus 구현체로 교체 가능해야 함 |
| `EventPool` | Application 포트 | 테스트 시 non-pooled Event 주입 가능해야 함 |
| `domain.EventProcessor` | Domain 계약 | Phase 2에서 WAL writer로 교체됨 |

**인터페이스 없이 concrete 참조를 유지하는 타입들:**

| 타입 | 위치 | 이유 |
|------|------|------|
| `*appingestion.IngestionService` | `HTTPHandler.svc` | HTTP handler는 이 서비스 외 다른 것으로 교체될 이유가 없음. 인터페이스 추가 시 ~2 ns dispatch 비용만 추가 |
| `*pool.EventPool` | `executor.WorkerPool.eventPool` | 같은 Infrastructure 레이어 내 import, 의존성 규칙 미위반 |

```go
// infrastructure/http/handler.go
// *IngestionService를 concrete type으로 보유. 인터페이스 dispatch 없음.
type HTTPHandler struct {
    svc *appingestion.IngestionService
}
```

**판단 기준 요약:**

1. 이 타입의 구현체가 Phase 2~4에서 교체될 가능성이 있는가?
2. 테스트에서 이 타입을 대체하는 더블(double)이 필요한가?
3. 인터페이스 추가 비용(~2 ns/call)이 그 유연성보다 작은가?

셋 중 하나라도 "그렇다"면 인터페이스를 도입한다. 모두 "아니다"면 concrete 참조를 유지한다.

### 4. Config 패턴

`NewWorkerPool(numWorkers, bufferDepth int, processor ..., pool ...)` 대신 Config struct를 사용한다.

```go
// infrastructure/executor/worker_pool.go
type Config struct {
    NumWorkers    int
    JobBufferSize int
    Processor     domain.EventProcessor
    EventPool     *pool.EventPool
}

func NewWorkerPool(cfg Config) *WorkerPool { ... }
```

**이유:**
- 인자 순서 오류를 컴파일 타임에 잡는다 (positional args는 런타임 버그 위험)
- Phase 2에서 `Config`에 `WALPath string` 필드를 추가할 때 호출 측 코드가 변경되지 않는다 (zero-value fallback)
- `cmd/main.go`에서 각 파라미터의 의미가 명확하다 (self-documenting)

### 5. sync.Pool 활용

`infrastructure/pool/event_pool.go`는 `sync.Pool`을 타입 안전한 래퍼로 감싼다.

```go
type EventPool struct {
    p sync.Pool
}

func (ep *EventPool) Acquire() *domain.Event {
    return ep.p.Get().(*domain.Event)  // 타입 단언 1회, 인터페이스 없음
}

func (ep *EventPool) Release(e *domain.Event) {
    e.Reset()   // Reset은 pool이 호출 — caller에 강제하지 않음
    ep.p.Put(e)
}
```

**왜 channel-based free list가 아닌 sync.Pool인가?**

채널 기반 풀은 참조를 영구적으로 보유한다. 트래픽이 낮을 때 N개의 Event 객체가 영원히 살아있어 메모리를 점유한다. `sync.Pool`은 GC-aware다: GC 사이클에서 드레인될 수 있다. 이는 idle 상태에서 메모리 누수를 방지하는 대신, 매 GC 후 첫 요청에서 재할당이 발생하는 트레이드오프를 수반한다. Phase 1의 sustained-load 시나리오에서 hit rate는 ~100%에 수렴하므로 이 트레이드오프는 수용 가능하다.

`Reset()`이 Release에서 호출되는 이유: caller에게 Reset 책임을 분산하면 누락될 수 있다. Pool 내부에서 강제하면 "Release하면 항상 zeroed"라는 불변식이 보장된다. 이전 요청의 데이터가 다음 요청으로 유출되는 것은 정확성 버그이자 잠재적 보안 취약점이다.

### 6. atomic counter

```go
// infrastructure/executor/worker_pool.go
type WorkerPool struct {
    ...
    processedCount atomic.Int64
    ...
}

func (wp *WorkerPool) work() {
    for e := range wp.jobCh {
        wp.processor.Process(e)
        wp.processedCount.Add(1)  // lock 없이 N개 goroutine에서 안전
        wp.eventPool.Release(e)
    }
}
```

`sync.Mutex`로 감싼 `int64` 대신 `atomic.Int64`를 사용한다. mutex는 OS 스케줄러 개입이 필요한 경우 ~100 ns 이상의 contention 비용을 유발한다. `atomic.Add`는 CPU 명령어 수준의 CAS 연산으로 ~5 ns다. N개 워커 goroutine 모두가 매 이벤트마다 이 카운터를 증가시키므로, 이 경로의 lock 제거는 직접적인 처리량 향상으로 이어진다.

### 7. HTTP Handler의 Concrete 참조 — 의도적 선택

```go
// infrastructure/http/handler.go
type HTTPHandler struct {
    svc *appingestion.IngestionService  // 인터페이스 아님
}
```

HTTP handler와 IngestionService의 관계는 1:1 고정 관계다. HTTP transport가 다른 Application service를 가리킬 시나리오가 존재하지 않는다. 이 관계를 인터페이스로 추상화하는 것은:

1. ~2 ns의 dispatch 비용 추가 (요청마다)
2. `IngestionServicePort`라는 의미 없는 인터페이스 추가
3. 테스트에서 해당 인터페이스의 mock 생성 추가

아무런 아키텍처적 유연성 없이 3가지 비용만 발생한다. 실용성을 선택한다.

이와 달리 `StatsHandler`는 `appingestion.Stats` 인터페이스를 받는다. Phase 4에서 Prometheus 구현체가 같은 포트를 구현하면 handler 코드는 변경 없이 동작하기 때문이다.

---

## Consequences

### 성능 이득

#### itab dispatch 최소화

```
HTTP accept path에서의 인터페이스 dispatch 횟수:

[HTTPHandler.ServeHTTP]
  → svc.Ingest()          : concrete 참조, 0 dispatch
    → pool.Acquire()      : EventPool 인터페이스,  ~2 ns
    → submitter.Submit()  : Submitter 인터페이스,  ~2 ns
    → pool.Release()      : EventPool 인터페이스,  ~2 ns (rejection path만)

합산: happy path ~4 ns, rejection path ~6 ns
```

모든 인터페이스를 사용했다면 `svc.Ingest()` dispatch ~2 ns 추가로 per-request 오버헤드가 늘어났을 것이다. 불필요한 추상화를 제거하여 hot path를 순수하게 유지한다.

#### Sentinel error로 rejection path 할당 제거

워커풀 포화 시 (= 가장 부하가 높은 순간):

```go
// BEFORE (가상): 동적 에러
return fmt.Errorf("saturated: queue depth %d, workers %d", depth, workers)
// → 매 rejection마다 string formatting + heap allocation → GC 자극

// AFTER: sentinel
return ErrOverloaded
// → 포인터 반환, 0 allocation
```

포화 상태에서 GC 자극을 피하는 것은 tail latency 안정성에 직결된다.

#### Hot path allocation: JSON decode 1회만

```
요청 처리 경로별 allocation 분석:

1. json.NewDecoder(r.Body)     : ~1 alloc (512-byte 내부 버퍼) — stdlib 한계
2. var req ingestRequest       : stack allocation (escape analysis 통과)
3. pool.Acquire()              : pool hit → 0 alloc / miss → 1 alloc
4. time.Now()                  : stack allocation
5. submitter.Submit()          : 채널 send, 0 alloc
6. ErrOverloaded               : 0 alloc (sentinel)

Net: pool hit 시 ~1 alloc/req (json.NewDecoder만)
     pool miss 시 ~2 alloc/req (json.NewDecoder + Event 신규 할당)
```

Phase 1 sustained-load에서 pool hit rate ~100%이므로 사실상 1 alloc/req.

#### GC pressure 최소화

`sync.Pool`의 GC-aware 특성과 zero-alloc happy path가 결합되어:
- 짧게 사는 객체(Event) 수가 최소화됨
- GC가 보는 live object 수가 줄어 mark phase 비용 감소
- GC trigger 빈도 억제 → STW pause 발생 간격 증가

### 확장성 (DDIA: Scalability)

Phase별 변경 범위가 명확하게 고립된다.

#### Phase 2: WAL Persistence

```go
// cmd/main.go — 변경은 이 1줄뿐
processor := wal.NewWriter(walPath)  // domain.EventProcessor 구현체

// executor/worker_pool.go: 무변경
// application/ingestion/service.go: 무변경
// infrastructure/http/handler.go: 무변경
```

WAL 작성 로직이 아무리 복잡해져도, HTTP handler는 단 한 글자도 바뀌지 않는다.

#### Phase 3: gRPC 수집 엔드포인트 추가

```go
// 신규 파일: infrastructure/grpc/handler.go
type GRPCHandler struct {
    svc *appingestion.IngestionService  // 동일 서비스 재사용
}

func (h *GRPCHandler) IngestEvent(ctx context.Context, req *pb.IngestRequest) (*pb.IngestResponse, error) {
    err := h.svc.Ingest(req.Source, req.Payload)
    // ... gRPC 상태 코드 변환
}

// application/ingestion/service.go: 무변경
// domain/event.go: 무변경
// infrastructure/http/handler.go: 무변경
```

비즈니스 규칙(validation, overload 처리)을 재작성할 필요가 없다.

#### Phase 4: Prometheus 메트릭

```go
// Stats 포트는 그대로. executor.WorkerPool도 그대로.
// infrastructure/http/stats.go만 교체:

// BEFORE: JSON handler
func StatsHandler(stats appingestion.Stats) http.HandlerFunc { ... }

// AFTER: Prometheus handler
func PrometheusHandler(stats appingestion.Stats) http.Handler {
    // prometheus/client_golang 사용
    // stats.ProcessedCount(), stats.QueueDepth() 동일하게 호출
}
```

`Stats` 포트 덕분에 executor.WorkerPool은 단 한 글자도 바뀌지 않는다.

### 감수하는 트레이드오프

#### Submitter/Stats/EventPool 인터페이스 dispatch 비용 ~2 ns 각

hot path에 인터페이스 dispatch가 존재한다. `Submitter.Submit()`과 `EventPool.Acquire()`는 각 요청마다 호출된다. 합산 ~4 ns의 추가 비용은 수용한다.

**수용 이유:**
- HTTP round-trip latency (~수십 μs)에서 4 ns는 noise level
- 이 비용이 Phase 2~4의 변경 고립에 대한 "보험료"
- 만약 profiling에서 이 경로가 진짜 bottleneck으로 나온다면, 인터페이스를 concrete로 교체하는 것은 3줄 변경으로 가능

#### executor ↔ pool 직접 import — 같은 레이어이므로 허용

```go
// infrastructure/executor/worker_pool.go
import "github.com/junyoung/core-x/internal/infrastructure/pool"
```

`executor`가 `pool`을 직접 참조한다. 두 패키지 모두 Infrastructure 레이어에 있으므로 Dependency Rule을 위반하지 않는다. 이를 Application 포트로 다시 매개하는 것은 불필요한 indirection이다.

**경계:** Infrastructure 내에서도 역방향 import(예: pool이 executor를 import)는 금지한다. 순환 의존이 발생하고, 테스트에서 한쪽 변경이 다른 쪽에 파급된다.

#### 완전한 DI보다 실용성 선택

`cmd/main.go`에 wiring 코드가 명시적으로 작성되어 있다. DI 컨테이너(wire, dig 등)를 사용하지 않는다.

**이유:**
- Phase 1의 의존성 그래프가 단순하다: 5개 객체, 직선적 의존
- DI 프레임워크는 생성 코드 자동화가 필요할 만큼 그래프가 복잡해질 때 도입한다
- 현재는 `cmd/main.go` 150줄이 전체 wiring을 명확하게 보여준다; 오히려 투명성이 높다

Phase 2~3에서 의존성이 복잡해지면 google/wire 도입을 재검토한다.

---

## Alternatives Considered

### 1. 모든 타입을 인터페이스로 감싸기

```go
// 고려한 방식:
type IngestionServicePort interface {
    Ingest(source, payload string) error
}
type HTTPHandler struct {
    svc IngestionServicePort  // 인터페이스
}
```

**거부 이유:**
- `HTTPHandler`가 `IngestionService` 외 다른 구현체를 가질 시나리오가 없다
- 인터페이스 추가는 mock 생성 부담을 늘린다
- itab dispatch ~2 ns가 추가되지만 얻는 유연성이 없다

YAGNI(You Aren't Gonna Need It). 인터페이스는 변경 가능성이 실재할 때 도입한다.

### 2. Submitter에 pool 포함 (Submit이 pool에서 Event를 획득)

```go
// 고려한 방식:
type Submitter interface {
    Submit(source, payload string) bool  // Event를 내부에서 획득
}
```

**거부 이유:**
- rejection path에서 pool에서 꺼낸 Event를 즉시 반환하는 로직이 Submitter 내부로 숨는다
- `IngestionService`가 pool 생명주기를 전혀 제어할 수 없게 된다
- 테스트에서 pool 거동을 독립적으로 검증할 수 없다
- 책임 분리가 불명확해지고 `EventPool` 포트의 존재 이유가 사라진다

명시적인 Acquire/Release가 pool 계약을 코드에서 가시화한다. "빌린 것은 반드시 반환한다"는 불변식을 코드 리뷰에서 추적 가능하게 만든다.

### 3. Domain이 transport layer를 아는 구조

```go
// 고려한 방식:
// domain/event.go에 HTTP 상태 코드 변환 로직 포함
func (e *Event) HTTPStatus() int { ... }
```

**거부 이유:**
- Domain이 HTTP를 알면, gRPC 추가 시 Domain에 gRPC 상태 코드 변환 로직도 추가해야 한다
- Domain이 transport에 오염되면, transport 교체 시 Domain 테스트도 변경된다
- HTTP 상태 코드는 wire format의 관심사다. Domain의 관심사는 "overloaded 여부"이지 "429인지 503인지"가 아니다

각 레이어가 자신의 관심사에만 집중해야 한다. Domain은 "무슨 일이 일어났는가"를, Infrastructure는 "그것을 어떤 프로토콜로 표현하는가"를 안다.

---

## Related Decisions

- [ADR-001: Pure Go 표준 라이브러리 선택](0001-use-pure-go-standard-library.md) — 외부 프레임워크 미사용으로 hidden allocation과 magic middleware chain 제거. 이 결정이 "Clean Architecture에서 stdlib만 의존"을 가능하게 한다.
- [ADR-002: Worker Pool & Sync.Pool 설계](0002-worker-pool-and-sync-pool-for-performance.md) — goroutine pool과 object pool의 구체적 구현 전략. ADR-003은 그 구현체들을 어떤 계층 구조로 배치하는지를 결정한다.

---

## Monitoring / Validation

이 아키텍처 결정이 의도대로 동작하는지 다음 지표로 검증한다.

| 지표 | 도구 | 임계값 | 경보 조건 |
|------|------|--------|-----------|
| Hot path alloc/req | `go test -bench -benchmem` | ≤ 1 alloc | > 1 alloc → pool miss 또는 신규 escape 발생 |
| GC pause (p99) | `runtime.ReadMemStats` | < 1 ms | > 5 ms → object retention 증가 |
| `queue_depth` | `GET /stats` | < `buffer_depth * 0.8` | 지속 포화 → worker 수 증가 검토 |
| `processed_total` 증분율 | `GET /stats` polling | 입력 이벤트 수와 일치 | 감소 → 무손실 위반 |
| Benchmark: `BenchmarkIngest` | `go test -bench` | < 1 μs/op | 증가 → hot path regression 조사 |

**Phase 경계 테스트**: 각 Phase 전환 시, 변경되어서는 안 되는 레이어의 파일이 실제로 변경되지 않았는지 git diff로 확인한다.

```bash
# Phase 2 WAL 도입 후: HTTP handler, Application service 무변경 검증
git diff main..phase2 -- internal/infrastructure/http/handler.go
git diff main..phase2 -- internal/application/ingestion/service.go
# 두 명령 모두 출력 없어야 함
```
