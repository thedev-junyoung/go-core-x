# ADR-002: Worker Pool & Sync.Pool — Goroutine Economy와 GC 압력 제어

- **Status**: Accepted
- **Date**: 2026-04-07
- **Deciders**: Core-X Principal Architect

---

## Context

### 요구사항

Core-X Phase 1의 성능 목표:

- **처리량**: 초당 수만 건(10k~100k events/sec)의 JSON 이벤트 무손실 수집
- **Latency**: HTTP accept → worker dispatch 경로 ~500 ns 이내 (p99)
- **GC pause**: 최소화, p99 < 1 ms
- **메모리**: 예측 가능한 상한, 트래픽 변동에 따른 메모리 폭발 없음
- **Backpressure**: 과부하 시 goroutine 누적 없이 즉각 HTTP 429 반환

이 목표들은 세 가지 독립적인 문제로 분해된다.

### 문제 1: Goroutine 폭발

가장 단순한 구현:

```go
// NAIVE: 요청당 goroutine 1개 생성
func (h *HTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    // ... decode request ...
    go process(event)  // 매 요청마다 새 goroutine
    w.WriteHeader(http.StatusAccepted)
}
```

이 방식의 문제:

**메모리**: Go goroutine은 시작 시 최소 2–8 KB의 스택을 할당한다. 10,000 동시 요청이면 최소 20–80 MB가 goroutine 스택만으로 소모된다. 트래픽 spike 시 goroutine 수가 제어되지 않으면 OOM이 발생한다.

**스케줄러 압박**: Go runtime의 M:N 스케줄러는 많은 수의 goroutine을 효율적으로 다루도록 설계되어 있지만, goroutine 수가 수십만에 달하면 scheduling overhead가 실제 처리 시간보다 커진다.

**Backpressure 없음**: `go process(event)`는 항상 성공한다. downstream이 느려도, 메모리가 부족해도, goroutine은 계속 생성된다. 과부하를 caller에게 신호로 돌려보낼 수단이 없다.

```
트래픽 100k req/sec, process() latency 10ms (가정):
  → 동시 실행 goroutine: 100k * 0.01 = 1,000개
  → 스택 메모리: 1,000 * 8KB = 8MB (스택만, heap 제외)
  → 합리적으로 보임

트래픽 spike: 1M req/sec (10x), process() latency 100ms (10x 느려짐):
  → 동시 실행 goroutine: 1M * 0.1 = 100,000개
  → 스택 메모리: 100k * 8KB = 800MB
  → heap은 별개 → OOM 가능성
```

### 문제 2: Allocation 폭발과 GC 압박

매 요청마다 `domain.Event`를 새로 할당한다면:

```go
// NAIVE: 매 요청마다 새 Event 할당
event := &domain.Event{  // heap allocation
    ReceivedAt: time.Now(),
    Source:     req.Source,
    Payload:    req.Payload,
}
```

`domain.Event`의 크기:

```go
type Event struct {
    ReceivedAt time.Time  // 24 bytes
    Source     string     // 16 bytes (header + pointer)
    Payload    string     // 16 bytes (header + pointer)
}
// 합계: 56 bytes + Source/Payload string data
```

**10k req/sec 기준:**
- Event 할당: 10k × 56 bytes = ~560 KB/sec
- String data (Source ~20B, Payload ~200B 가정): 10k × 220 bytes = ~2.2 MB/sec
- 합계: ~2.8 MB/sec의 short-lived object 생성

Go GC는 이 객체들을 young generation에서 수집하지만 (Go의 GC는 generational이 아니라 tri-color concurrent mark-sweep이다), **allocation rate가 높을수록 GC trigger 빈도가 증가한다.**

Go의 GC trigger 조건: heap size가 이전 GC 시점의 N배(기본 `GOGC=100`, 즉 2배)가 되면 GC가 시작된다. allocation rate가 높으면:
1. heap이 빠르게 커진다
2. GC가 자주 실행된다
3. GC의 mark phase에서 STW(Stop-The-World) pause가 발생한다
4. 모든 goroutine이 일시 정지 → latency spike

### 문제 3: Channel-based 큐 vs. 다른 동기화 메커니즘

Worker pool 구현에서 job queue를 어떻게 구현하는가?

**선택지:**
1. `sync.Mutex` + slice/linked list: 전통적 job queue
2. Buffered channel: Go native, scheduler-friendly
3. Lock-free ring buffer: 최고 성능, 구현 복잡

---

## Decision

**두 가지 메커니즘을 조합한다:**
1. **고정 크기 Worker Pool**: N개 goroutine + 1개 buffered channel 큐
2. **sync.Pool**: `domain.Event` 객체 재사용 (malloc 최소화)

### 결정 1: 고정 크기 Worker Pool (channel-based)

#### 설계

```
         HTTP Handler goroutines (N개, net/http가 관리)
               │
               │ Submit(*Event)  [non-blocking]
               ▼
    ┌─────────────────────────────────────┐
    │  buffered channel  cap=JobBufferSize │  ← backpressure point
    │  chan *domain.Event                  │
    └─────────────────────────────────────┘
               │
               │ for e := range jobCh
               ▼
    ┌───────────────────────────────────────┐
    │  Worker goroutine 1                   │
    │  Worker goroutine 2                   │
    │  ...                                  │
    │  Worker goroutine N  (고정, 불변)     │  ← goroutine 수 상한
    └───────────────────────────────────────┘
```

#### 구현

```go
// infrastructure/executor/worker_pool.go

type WorkerPool struct {
    jobCh          chan *domain.Event  // backpressure 채널
    wg             sync.WaitGroup
    processedCount atomic.Int64        // lock-free 카운터
    processor      domain.EventProcessor
    eventPool      *pool.EventPool
}

type Config struct {
    NumWorkers    int                  // goroutine 수 (고정)
    JobBufferSize int                  // 채널 깊이
    Processor     domain.EventProcessor
    EventPool     *pool.EventPool
}
```

#### 왜 Buffered Channel인가?

**Scheduler-friendly**: Go 채널은 goroutine 스케줄러와 직접 통합되어 있다. 채널이 비면 goroutine이 park(대기)되고, 채널에 데이터가 들어오면 스케줄러가 해당 goroutine을 즉시 runnable 상태로 전환한다. mutex + condvar 기반 구현과 달리, OS syscall 없이 Go runtime 수준에서 처리된다.

```
Mutex + condvar 기반 wake:
  mutex.Lock() → 커널 공간 진입 가능 → OS 스케줄러 개입 → ~100–1000 ns

Channel-based wake:
  Go runtime 스케줄러 내부 처리 → ~50–200 ns
```

**가시성**: `len(jobCh)`는 현재 큐 깊이를 O(1)로 반환한다. `/stats` 엔드포인트가 이 값을 직접 노출한다. mutex 기반 큐에서는 별도 카운터가 필요하다.

**Backpressure semantics**: buffered channel이 가득 차면 `select { default: return false }`가 즉각 실행된다. 이것이 HTTP 429 반환의 트리거다.

```go
// infrastructure/executor/worker_pool.go
func (wp *WorkerPool) Submit(e *domain.Event) bool {
    select {
    case wp.jobCh <- e:
        return true
    default:
        // 채널이 가득 참 → non-blocking, 즉각 false 반환
        // → caller(IngestionService)가 ErrOverloaded 반환
        // → HTTP handler가 429 응답
        return false
    }
}
```

`select { default: ... }` 패턴은 Go에서 non-blocking channel operation의 표준 관용구다. `default` 분기는 다른 case가 즉시 실행될 수 없을 때 실행되며, goroutine을 block하지 않는다.

#### 왜 N개 고정인가?

**Scalability의 핵심은 unbounded resource 제거다 (DDIA).**

"goroutine은 가벼우니 많이 만들어도 된다"는 착각이 있다. goroutine이 가벼운 것은 thread 대비 상대적으로 가볍다는 의미다. 무제한으로 생성해도 된다는 의미가 아니다.

```
고정 N개의 효과:
  - goroutine 스택 메모리 상한 = N × 8KB (예측 가능)
  - Go runtime 스케줄러 부하 상한 = N
  - CPU core당 goroutine 수 = N/GOMAXPROCS (균등 분배)

무제한 goroutine의 문제:
  - 메모리 상한 없음
  - 스케줄러 pressure 무제한
  - 과부하 시 goroutine 생성 속도 > 처리 속도 → 메모리 소진
```

**초기 설정값: `runtime.NumCPU() * 2`**

```go
// cmd/main.go
numWorkers := runtime.NumCPU() * 2
```

Go 워커가 CPU-bound가 아닌 경우(I/O wait가 있는 경우) `NumCPU * 2`가 합리적인 시작점이다. Phase 1의 processor는 `slog.Info` 호출 (I/O) + `processedCount.Add(1)` (CPU)로 구성되므로, 약간의 multiplier가 CPU utilization을 높인다.

**실제 튜닝 방법:**

```bash
# QueueDepth가 지속적으로 높으면 → NumWorkers 증가 필요
# QueueDepth가 항상 0이면 → NumWorkers가 충분 (또는 과잉)
curl http://localhost:8080/stats
# {"queue_depth": N, "processed_total": M}
```

#### Backpressure 동작

```
정상 상태:
  jobCh [===      ] 30% full
  Submit() → case jobCh <- e: success (true)
  HTTP 202 Accepted

과부하 상태:
  jobCh [=========] 100% full
  Submit() → default: (non-blocking) return false
  IngestionService → ErrOverloaded
  HTTP 429 Too Many Requests
  → slog.Warn 로그 기록
  → 새 goroutine 생성 없음
  → Event 즉시 pool에 반환
```

429를 받은 caller는 exponential backoff + retry로 대응해야 한다. 이것이 DDIA Reliability의 핵심: 과부하를 조용히 삼키지 말고 명시적 신호로 돌려보낸다.

#### Graceful Shutdown

```go
// infrastructure/executor/worker_pool.go
func (wp *WorkerPool) Shutdown(ctx context.Context) {
    // 1. 채널 닫기: 새 Submit() 차단 (send to closed channel → panic)
    //    → Shutdown 전에 HTTP server가 먼저 멈춰야 함
    close(wp.jobCh)

    // 2. 채널에 남은 Event 모두 drain될 때까지 대기
    done := make(chan struct{})
    go func() {
        wp.wg.Wait()  // 모든 worker goroutine이 for range 루프를 빠져나올 때까지
        close(done)
    }()

    select {
    case <-done:
        // 정상 종료: 모든 이벤트 처리 완료
    case <-ctx.Done():
        // timeout: 일부 이벤트 미처리 가능 → slog.Warn
    }
}
```

**shutdown 순서 불변식:**

```
cmd/main.go의 shutdown 순서:
  1. OS signal (SIGINT/SIGTERM) 수신
  2. http.Server.Shutdown(ctx) — 새 accept 거부, in-flight 요청 완료 대기
  3. workerPool.Shutdown(ctx)  — 채널 drain, worker 종료
```

이 순서를 지키지 않으면 `close(wp.jobCh)` 이후 HTTP handler의 `Submit()`이 panic한다.

### 결정 2: sync.Pool을 통한 Event 객체 재사용

#### 설계

```
요청 처리 흐름:

HTTP Handler
  │
  ├─► pool.Acquire()  ──→  sync.Pool.Get()
  │       │                    ├─ pool hit:  기존 *Event 반환 (0 alloc)
  │       │                    └─ pool miss: new(domain.Event) (1 alloc)
  │       │
  │       ▼
  │   *domain.Event 채워넣기
  │   (ReceivedAt, Source, Payload)
  │
  ├─► submitter.Submit(e)
  │       │
  │       ▼
  │   buffered channel에 *Event 포인터 전송
  │
  └─► (HTTP 응답: 202)

Worker goroutine
  │
  ├─► processor.Process(e)
  │
  └─► pool.Release(e)  ──→  e.Reset() + sync.Pool.Put(e)
                                (다음 요청에서 재사용)
```

#### 구현

```go
// infrastructure/pool/event_pool.go

type EventPool struct {
    p sync.Pool
}

func New() *EventPool {
    return &EventPool{
        p: sync.Pool{
            New: func() any {
                return &domain.Event{}  // miss 시 1회만 alloc
            },
        },
    }
}

func (ep *EventPool) Acquire() *domain.Event {
    return ep.p.Get().(*domain.Event)  // type assertion: ~1 ns
}

func (ep *EventPool) Release(e *domain.Event) {
    e.Reset()       // 필드 초기화 (보안 불변식)
    ep.p.Put(e)     // pool에 반환
}
```

```go
// domain/event.go
func (e *Event) Reset() {
    e.ReceivedAt = time.Time{}
    e.Source = ""
    e.Payload = ""
}
```

#### 왜 sync.Pool인가? Channel-based free list와의 비교

**Channel-based free list:**

```go
// ALTERNATIVE: channel 기반 free list
type EventPool struct {
    pool chan *domain.Event
}

func (p *EventPool) Acquire() *domain.Event {
    select {
    case e := <-p.pool:
        return e
    default:
        return &domain.Event{}
    }
}

func (p *EventPool) Release(e *domain.Event) {
    e.Reset()
    select {
    case p.pool <- e:
    default:
        // pool 가득 참, 그냥 버림
    }
}
```

이 방식의 문제:

1. **메모리 누수**: channel에 들어간 `*Event`는 GC가 수집할 수 없다. 트래픽이 줄어도 N개의 Event가 영원히 heap에 살아있다.
2. **Pool 크기 고정**: 채널 cap이 pool 크기 상한이다. 버스트 후 idle 상태에서 메모리를 과점한다.
3. **False sharing 가능성**: channel의 internal lock이 hot path에 영향을 줄 수 있다.

**sync.Pool의 장점:**

```
GC-aware: sync.Pool은 GC 사이클마다 drain될 수 있다.
  → idle 상태: GC가 pool을 비움 → 메모리 자동 반환
  → sustained load: hit rate ~100% → allocation 사실상 0
  → 트레이드오프: GC 직후 첫 요청은 pool miss → 1 alloc
```

Phase 1의 타겟 시나리오(sustained high-throughput load)에서 pool hit rate는 ~100%에 수렴한다. GC 직후 잠깐의 miss는 noise level이다.

**sync.Pool의 내부 동작:**

```
Go runtime의 sync.Pool 구현:
  - P(processor)별 독립 슬롯 유지 (P = GOMAXPROCS 수)
  - Acquire: 현재 P의 슬롯에서 꺼냄 (lock-free)
  - Release: 현재 P의 슬롯에 넣음 (lock-free)
  - P당 슬롯이 가득 차면 victim cache로 이동
  - GC 시 victim cache → 수집 대상

결과:
  - Acquire/Release: ~10–20 ns (lock-free, per-P isolation)
  - Cache line ping-pong 없음 (P별 분리)
  - GC-aware memory management 자동 적용
```

#### Reset()은 누가 호출하는가 — Release의 책임

```go
// Release에서 Reset() 호출 — caller 책임 아님
func (ep *EventPool) Release(e *domain.Event) {
    e.Reset()       // ← 여기서만 호출
    ep.p.Put(e)
}
```

**왜 Release에서 호출하는가?**

caller가 Reset()을 직접 호출하게 하면:

```go
// ALTERNATIVE (나쁜 패턴):
func (wp *WorkerPool) work() {
    for e := range wp.jobCh {
        wp.processor.Process(e)
        wp.processedCount.Add(1)
        e.Reset()           // caller가 Reset 책임
        wp.eventPool.Release(e)  // pool 반환
    }
}
```

이 방식에서 Reset()이 누락되면 이전 요청의 데이터가 다음 요청으로 유출된다. `Source: "user-A"`가 `Source: "user-B"`의 Event 처리에 섞이는 것은 정확성 버그이자 잠재적 보안 취약점이다.

`Release()` 내부에서 강제하면 "Release하면 반드시 zeroed"라는 불변식이 타입 경계에서 보장된다. caller는 Reset을 기억할 필요가 없다.

### 두 메커니즘의 시너지

```
Workers 처리 중:
  채널 버퍼:    [e1][e2][e3][  ][  ]   3개 Event 대기
  Pool:         [e4][e5]               2개 Event 재사용 가능
  Worker 1 처리 중: e6 (Process() 실행)
  Worker 2 처리 중: e7 (Process() 실행)

GC가 보는 live objects:
  채널 버퍼의 Event 포인터들 (3개)
  + Worker에서 처리 중인 Event 포인터 (2개)
  + Pool에 있는 Event (2개, GC가 수집 대상으로 볼 수 있음)
  ---
  총 5–7개 (고정)

무제한 goroutine + 매번 alloc 방식:
  동시 처리 1000개라면 → 1000개의 Event가 heap에 live
  GC가 traverse해야 하는 object 수: 1000개
  → mark phase 비용: ~1000배 증가
```

고정 worker + pooled Event의 조합은 GC가 보는 live object 수를 `NumWorkers + JobBufferSize` 수준으로 고정한다. 처리량이 10x 증가해도 live object 수는 동일하다.

---

## GC Optimization Mechanism (심층 분석)

### 1. Object Allocation Rate 감소

```
Without pool (10k req/sec):
  Event alloc:  10,000/sec × 56 bytes = 560 KB/sec
  String data:  10,000/sec × ~220 bytes = ~2.2 MB/sec
  Total:        ~2.8 MB/sec allocation rate

With pool (10k req/sec, hit rate ~100%):
  Event alloc:  ~0/sec (pool hit, 0 new allocation)
  String data:  Source/Payload는 여전히 alloc
  Total:        ~2.2 MB/sec → Event struct 560 KB/sec 절감
```

Event struct 자체의 할당이 사라진다. string data는 JSON decode 과정에서 stdlib이 할당하므로 Pool로 제거할 수 없다 (zero-copy JSON은 별도 ADR에서 다룬다).

### 2. GC가 보는 Live Object 수 감소

```go
// Go GC는 heap의 live object를 모두 traverse (mark phase)
// object 수 ↓ → mark phase cost ↓ → STW pause time ↓

// Pool 미사용: 처리 중인 모든 Event가 heap에 live
//   N개 goroutine × 처리 시간 동안 = N개의 Event live
//   N = 트래픽에 비례 → unbounded

// Pool 사용: Event는 채널 버퍼 + worker goroutine에만 live
//   live count = JobBufferSize + NumWorkers = fixed
//   트래픽 10x 증가 → live object count 동일
```

### 3. Mark Phase 비용 감소

```
Go GC tri-color mark-sweep:
  1. STW: GC root 스캔 시작 (짧음, ~10–100 μs)
  2. Concurrent mark: goroutine과 동시에 heap 객체 traverse
  3. STW: mark termination (짧음, ~10–100 μs)
  4. Concurrent sweep: unreachable object 메모리 반환

mark phase 비용 ∝ live heap object 수

Pool 사용으로 live object ↓:
  → mark phase duration ↓
  → CPU time for GC ↓ (application에 더 많은 CPU)
  → STW pause 발생 빈도 ↓ (GC trigger 자체가 덜 발생)
```

### 4. GC Trigger 빈도 감소

```
Go GC trigger (기본 GOGC=100):
  heap size가 이전 GC 종료 시점의 2배가 되면 GC 시작

  Without pool:
    alloc rate: 2.8 MB/sec
    heap 2x에 걸리는 시간: heap_size / 2.8 MB/sec
    → heap이 작으면 GC가 매우 자주 실행

  With pool:
    alloc rate: ~2.2 MB/sec (Event struct 절감)
    GC trigger 빈도: 약 21% 감소
    → 더 중요: Event가 즉시 반환되므로 heap growth 자체가 느림
```

### 5. 예상 성능 수치

```
Without pool, without fixed worker:
  alloc/req:    ~10 (Event + goroutine internals + 채널 관련)
  GC frequency: 높음 (alloc rate에 비례)
  p99 latency:  GC pause 포함 ~수 ms 가능
  goroutine peak: 트래픽에 비례, unbounded

With pool + fixed worker (Core-X 현재):
  alloc/req:    ~1 (json.NewDecoder만)
  GC frequency: 낮음 (alloc rate 최소화)
  p99 latency:  ~수백 μs (GC pause 크게 감소)
  goroutine:    NumWorkers + net/http worker = fixed + ~O(concurrent conns)
```

---

## Consequences

### 성능 이득 (DDIA: Reliability)

**Goroutine 수 상한 확보:**

```go
// cmd/main.go
workerPool := executor.NewWorkerPool(executor.Config{
    NumWorkers:    runtime.NumCPU() * 2,
    JobBufferSize: runtime.NumCPU() * 2 * 10,
    // ...
})
```

시스템이 다루는 최대 goroutine 수가 `NumCPU*2 + net/http worker pool`로 상한이 정해진다. OOM으로 이어지는 goroutine 폭발이 구조적으로 불가능하다.

**GC pause 감소:**

pool hit rate ~100% sustained load에서:
- Event struct 할당 제거 → alloc rate 감소 → GC trigger 빈도 감소
- live object 수 고정 → mark phase 비용 고정 → STW pause 시간 예측 가능

**Backpressure 명시화:**

```go
// 과부하의 3가지 신호가 모두 명시적:
// 1. Submit() false → ErrOverloaded (정확성)
// 2. HTTP 429 (caller에게 신호)
// 3. slog.Warn 로그 (운영자에게 신호)
```

### 감수하는 비용

#### Graceful Shutdown 복잡성

```go
// shutdown 순서를 반드시 지켜야 함:
// 1. http.Server.Shutdown() → new accept 차단
// 2. workerPool.Shutdown()  → drain → workers 종료

// 순서 위반 시:
workerPool.Shutdown()  // ← close(jobCh) 호출
// 이후 in-flight HTTP 요청이 Submit() 시도
wp.jobCh <- e  // ← closed channel에 send → PANIC
```

이 순서는 `cmd/main.go`의 shutdown signal handler에 명시적으로 구현되어 있다. 단순성을 희생했지만, 이 복잡성은 "데이터 유실 없는 종료"를 위한 필수 비용이다.

#### 워커 수 튜닝 의존성

`NumWorkers`와 `JobBufferSize`의 최적값은 load testing 없이는 알 수 없다.

```
NumWorkers 너무 작음:
  → jobCh 빠르게 포화
  → 429 발생 빈도 높음
  → 처리량 저하

NumWorkers 너무 큼:
  → goroutine stack 메모리 낭비
  → 스케줄러 pressure 증가
  → context switching 오버헤드

JobBufferSize 너무 작음:
  → burst 흡수 못함
  → 순간적 트래픽 spike에서 과도한 429

JobBufferSize 너무 큼:
  → memory overhead (각 slot은 *Event pointer, 8 bytes)
  → backpressure 신호가 늦게 전달됨 (큐가 차는 데 시간)
```

**운영 가이드라인:**

```bash
# 현재 상태 확인
watch -n 1 'curl -s http://localhost:8080/stats'
# queue_depth가 지속적으로 JobBufferSize의 80% 이상 → NumWorkers 증가 필요
# queue_depth가 항상 0 → NumWorkers 과잉 가능

# Go runtime 내부 확인
go tool pprof http://localhost:6060/debug/pprof/goroutine
# goroutine 수가 NumWorkers + 예상 overhead를 크게 초과 → 의도치 않은 goroutine 생성 확인
```

#### sync.Pool의 GC drain 트레이드오프

GC 직후 pool이 drain되면 첫 요청들은 pool miss → 1 alloc/req. 이 현상은:
- Phase 1 sustained-load에서는 무시 가능
- idle → burst transition 패턴에서는 잠깐의 latency spike 가능

**완화 방법 (필요시):**

```go
// pool warmup: GC 후 재사용 가능한 객체를 pool에 미리 채움
// (Phase 1에서는 필요 없음, Phase 4에서 부하 패턴이 명확해지면 검토)
func (ep *EventPool) Warmup(n int) {
    events := make([]*domain.Event, n)
    for i := range events {
        events[i] = ep.Acquire()
    }
    for _, e := range events {
        ep.Release(e)
    }
}
```

---

## Alternatives Considered

### 1. Unbounded Goroutine (요청당 1 goroutine)

```go
// REJECTED
go process(event)
```

**거부 이유:**
- Goroutine 수 상한 없음 → OOM 가능
- Backpressure 없음 → downstream 느려도 goroutine 계속 생성
- 메모리 footprint 예측 불가 (DDIA Reliability 위반)

### 2. Thread Pool (Java/C++ 스타일 — OS thread 기반)

Go에서 OS thread pool을 명시적으로 구현하는 것은 `runtime.LockOSThread()` + 복잡한 lifecycle 관리가 필요하다.

**거부 이유:**
- Go goroutine이 OS thread보다 훨씬 가볍다 (2 KB vs 1 MB 초기 스택)
- M:N 스케줄러가 goroutine을 OS thread에 효율적으로 매핑
- OS thread 직접 관리 = Go runtime과의 충돌 위험

### 3. Lock-free Ring Buffer (Disruptor 패턴)

```
Disruptor 패턴 (Java 기원, LMAX에서 개발):
  - CAS-based sequence counter
  - Cache line padding으로 false sharing 제거
  - Multi-producer, multi-consumer lock-free queue
  - 이론적 처리량: ~수백만 ops/sec
```

**고려 이유:** 최고 성능.

**거부 이유:**
- Go 채널이 이미 ~10–50 ns/op 수준이다. Phase 1의 bottleneck은 JSON decode (~800 ns)이지 channel operation이 아니다.
- 구현 복잡도가 크게 증가한다. lock-free 자료구조는 ABA problem, memory ordering, false sharing을 모두 고려해야 한다.
- **DDIA Maintainability 위반**: 복잡한 lock-free 코드는 디버깅, 리뷰, 수정이 어렵다.
- 측정 없이 최적화하는 것은 premature optimization이다. Profiling에서 channel이 실제 bottleneck으로 확인되면 그때 검토한다.

**도입 조건**: `go test -bench -benchmem`에서 channel operation이 전체 latency의 20% 이상을 차지하는 것이 측정으로 확인될 때.

### 4. Single-threaded Event Loop (Node.js 스타일)

```
단일 goroutine이 모든 이벤트를 처리:
  - I/O multiplexing (epoll/kqueue)
  - Non-blocking 처리
```

**거부 이유:**
- Go는 multi-core를 `GOMAXPROCS`로 자연스럽게 활용한다. 단일 event loop는 이 이점을 포기한다.
- CPU-bound 작업이 event loop를 블로킹하면 전체 처리량 저하.
- Phase 2에서 WAL write (I/O + CPU)가 추가되면 단일 loop 모델은 즉각 한계에 부딪힌다.
- Go의 goroutine model이 event loop보다 표현력이 높고 이해하기 쉽다.

### 5. Channel-based Free List (sync.Pool 대체)

앞서 Context에서 상세 비교. 요약:
- 메모리 누수 위험 (idle 상태에서 N개 Event 영구 보유)
- GC-aware 없음
- pool 크기 수동 튜닝 필요

---

## Related Decisions

- [ADR-001: Pure Go 표준 라이브러리 선택](0001-use-pure-go-standard-library.md) — 외부 프레임워크 없이 net/http를 직접 사용하므로, HTTP handler의 goroutine lifecycle이 명확하다. 프레임워크의 숨겨진 worker pool과 충돌 없이 executor.WorkerPool이 독립적으로 동작한다.
- [ADR-003: Clean Architecture + Zero-Allocation](0003-clean-architecture-with-zero-allocation.md) — WorkerPool은 `Submitter`와 `Stats` 포트를 구현하는 Infrastructure 컴포넌트다. EventPool은 `EventPool` 포트를 구현한다. 이 ADR은 성능 메커니즘을, ADR-003은 그 메커니즘들이 어느 계층에 배치되는지를 결정한다.

---

## Monitoring / Validation

### 벤치마크

```bash
# Worker pool 처리량 (allocation 포함)
go test ./internal/infrastructure/executor/... -bench=BenchmarkSubmit -benchmem -count=5

# End-to-end ingestion (HTTP handler → pool → worker)
go test ./... -bench=BenchmarkIngest -benchmem -count=5
# 기대값: ~1 alloc/op, ~1 μs/op
```

### 런타임 지표

| 지표 | 엔드포인트/도구 | 임계값 | 경보 조건 |
|------|----------------|--------|-----------|
| `queue_depth` | `GET /stats` | < `JobBufferSize * 0.8` | 지속 포화 → `NumWorkers` 증가 검토 |
| `processed_total` 증분 | `GET /stats` 폴링 | 입력 이벤트 수와 일치 | 감소 → 무손실 위반 |
| Goroutine 수 | `pprof/goroutine` | `NumWorkers + 50` 이하 | 초과 → goroutine 누수 조사 |
| GC pause (p99) | `runtime.ReadMemStats` | < 1 ms | > 5 ms → pool miss 증가 또는 대규모 alloc 발생 |
| Alloc/op | `-benchmem` | ≤ 1 | > 1 → pool escape 또는 신규 alloc 경로 확인 |

### GC 동작 확인

```bash
# GODEBUG로 GC trace 출력
GODEBUG=gctrace=1 ./core-x 2>&1 | grep "^gc"
# 출력 예:
# gc 1 @0.5s 0%: 0.01+0.5+0.01 ms clock, ...
# gc 2 @1.1s 0%: 0.01+0.4+0.01 ms clock, ...
# → GC pause (중간 숫자)가 < 1ms 유지되어야 함
# → GC frequency (숫자 간격)가 규칙적이어야 함

# pool hit/miss 간접 확인
# alloc/op이 벤치마크에서 ~1 유지 → pool hit rate 높음
# alloc/op이 증가 → pool pressure 증가 → NumWorkers/BufferSize 재검토
```

### Phase 경계 검증

```bash
# Phase 2: WAL processor 교체 후 worker pool 무변경 확인
git diff main..phase2 -- internal/infrastructure/executor/worker_pool.go
# 출력 없어야 함 (domain.EventProcessor 구현체만 교체)

# Phase 2: pool 무변경 확인
git diff main..phase2 -- internal/infrastructure/pool/event_pool.go
# 출력 없어야 함
```
