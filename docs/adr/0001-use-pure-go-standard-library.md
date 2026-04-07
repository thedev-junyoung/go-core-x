# ADR-001: Pure Go 표준 라이브러리 선택 — 외부 HTTP 프레임워크 미사용

- **Status**: Accepted
- **Date**: 2026-04-07
- **Deciders**: Core-X Principal Architect

---

## Context

### 요구사항 및 업계 관행

Core-X Phase 1의 첫 번째 결정은 HTTP 수집 서버를 어떤 기반 위에 구축할 것인가다. 초당 수만 건의 JSON 이벤트를 처리하는 고성능 수집 엔드포인트 `POST /ingest`가 필요하다.

**업계 표준 선택지:**

| 프레임워크 | 특징 | GitHub Stars (2026) |
|---|---|---|
| Gin | 가장 널리 쓰이는 Go HTTP 프레임워크. Radix tree router, middleware chain | ~80k |
| Echo | Gin과 유사. 미들웨어 에코시스템 풍부 | ~30k |
| Fiber | Fasthttp 기반. net/http와 호환 안 됨, 최고 벤치마크 수치 | ~35k |
| Chi | net/http 호환, 가볍고 composable | ~18k |
| `net/http` | Go 표준 라이브러리. 외부 의존 없음 | — |

거의 모든 팀이 Gin 또는 Echo를 선택한다. 이유는 명확하다: 개발 속도, 에코시스템, 익숙함.

**그렇다면 왜 표준 라이브러리를 선택해야 하는가?**

이 프로젝트는 두 가지 목표를 동시에 추구한다:

1. **성능 목표**: hot path allocation 최소화, GC pause 억제, 예측 가능한 p99 latency
2. **학습 목표**: Go 언어와 DDIA 원칙에 대한 깊은 이해, hidden abstraction 없이 성능을 통제하는 능력

두 목표 모두 외부 프레임워크와 긴장 관계에 놓인다.

### 문제: 프레임워크의 숨겨진 비용

"Gin이 net/http보다 빠르다"는 벤치마크는 흔히 오해를 낳는다. Gin은 radix tree routing을 제공하므로 라우트가 수백 개인 애플리케이션에서 routing 오버헤드를 줄인다. 그러나 Core-X는 Phase 1에서 라우트가 2개(`/ingest`, `/stats`)다.

Gin이 제공하는 편의성은 공짜가 아니다.

#### Gin의 숨겨진 할당 비용

```go
// Gin 내부: gin.Context는 sync.Pool에서 꺼내지만
// 여전히 여러 내부 slice를 초기화한다.

// gin/context.go (참조)
type Context struct {
    writermem responseWriter
    Request   *http.Request
    Writer    ResponseWriter

    Params   Params        // []Param — slice, heap allocation
    handlers HandlersChain // []HandlerFunc — slice, heap allocation
    index    int8
    fullPath string

    engine       *Engine
    params       *Params
    skippedNodes *[]skippedNode

    // ...
    Keys     map[string]any  // lazy-initialized map
    Errors   errorMsgs       // slice
    Accepted []string        // slice
    // ...
}
```

`gin.Context`는 pooled지만, 각 요청마다 내부 slice들이 초기화되거나 재설정된다. `HandlersChain`(미들웨어 체인)은 slice다. 미들웨어가 3개면 최소 3-element slice가 per-context로 존재한다.

#### Middleware chain의 비용

```go
// Gin의 middleware 실행 방식 (단순화)
func (c *Context) Next() {
    c.index++
    for c.index < int8(len(c.handlers)) {
        c.handlers[c.index](c)  // indirect function call
        c.index++
    }
}
```

각 미들웨어는 slice 인덱싱 + indirect function call로 실행된다. 미들웨어 3개이면 hot path에 3번의 indirect call이 추가된다. 각각 ~2–5 ns. 합산 ~10–15 ns가 **미들웨어 dispatch 오버헤드만으로** 발생한다.

Core-X의 `ServeHTTP`는 다음과 같다:

```go
// infrastructure/http/handler.go
func (h *HTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    var req ingestRequest
    dec := json.NewDecoder(r.Body)
    // ... decode, validate, call svc.Ingest()
}
```

미들웨어 없음. dispatch 오버헤드 없음. 코드가 하는 일이 코드에서 그대로 보인다.

#### Reflection 기반 binding

Gin의 `c.ShouldBindJSON(&req)`는 내부적으로 `encoding/json`을 사용하지만, 타입을 `reflect.Type`으로 캐시하고 validator 태그를 파싱한다. 이 경로에는 reflect 호출과 map lookup이 포함된다.

```go
// net/http + stdlib json의 경우:
dec := json.NewDecoder(r.Body)
dec.Decode(&req)
// → 순수 json.Decoder, reflect 캐시 없음
```

Core-X에서는 JSON decode 자체가 1 alloc/req의 주요 원인이다. 그 위에 binding magic을 더하는 것은 정당화되지 않는다.

#### 프로파일링의 복잡성

```
Gin을 사용하는 경우 pprof 결과 예시:
  net/http.(*Server).Serve
    gin.(*Engine).ServeHTTP
      gin.(*Context).Next          ← Gin 내부
        gin.Recovery()             ← middleware
          gin.Logger()             ← middleware
            yourHandler()          ← 실제 비즈니스 로직

net/http만 사용하는 경우:
  net/http.(*Server).Serve
    yourHandler.ServeHTTP          ← 즉시 비즈니스 로직
```

성능 문제가 발생했을 때 Gin의 내부 스택을 추적해야 한다면, 문제의 원인이 프레임워크 내부인지 비즈니스 로직인지 구분하는 데 비용이 든다. `net/http`만 사용하면 call stack이 단순하다.

### 문제: Fiber의 경우

Fiber는 가장 높은 벤치마크 수치를 보인다. 이유는 `net/http`를 우회하고 `fasthttp`를 직접 사용하기 때문이다.

**거부 이유:**

1. **net/http 비호환**: Go 표준 생태계의 모든 미들웨어, 도구, 라이브러리는 `http.Handler`를 기준으로 구성된다. Fiber는 이 생태계에서 벗어난다.
2. **숨겨진 비용 증가**: `fasthttp`는 `net/http`의 allocations를 다른 방식으로 처리한다. 처음에는 빠르게 느껴지지만, 실제 workload에서 차이는 미미하다.
3. **학습 목표 위반**: Fiber의 "빠름"은 Go 표준 라이브러리를 깊이 이해하는 것으로 얻는 빠름이 아니다. 프레임워크를 바꿔서 얻는 빠름이다. Core-X의 목표는 후자가 아니다.
4. **go.mod 오염**: `fasthttp` 의존성이 go.mod에 추가된다. 이후 모든 도구 호환성, 버전 충돌, 취약점 스캔 범위가 넓어진다.

---

## Decision

**Go 표준 라이브러리 `net/http`만 사용한다. go.mod에 non-stdlib HTTP 관련 의존성을 도입하지 않는다.**

```
module github.com/junyoung/core-x

go 1.25.5
```

go.mod에 `require` 블록이 없다. 이것이 의도된 상태다.

### 구체적 적용

#### Router: net/http의 ServeMux

```go
// cmd/main.go
mux := http.NewServeMux()
mux.Handle("POST /ingest", httpinfra.NewHTTPHandler(svc))
mux.HandleFunc("GET /stats", httpinfra.StatsHandler(workerPool))

srv := &http.Server{
    Addr:         cfg.Addr,
    Handler:      mux,
    ReadTimeout:  5 * time.Second,
    WriteTimeout: 10 * time.Second,
    IdleTimeout:  60 * time.Second,
}
```

Go 1.22+의 `ServeMux`는 메서드 + 경로 패턴을 지원한다 (`"POST /ingest"`). 라우트가 2개인 Phase 1에서 radix tree router가 필요하지 않다.

#### Handler: http.Handler 인터페이스 직접 구현

```go
// infrastructure/http/handler.go
type HTTPHandler struct {
    svc *appingestion.IngestionService
}

func (h *HTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    // 이 함수에서 일어나는 모든 일이 가시적이다.
    // Gin Context 없음, middleware chain 없음, reflect binding 없음.
    var req ingestRequest
    dec := json.NewDecoder(r.Body)
    dec.DisallowUnknownFields()
    if err := dec.Decode(&req); err != nil {
        http.Error(w, "invalid request body", http.StatusBadRequest)
        return
    }
    // ...
}
```

`http.Handler` 인터페이스를 직접 구현한다. 이 패턴은 Go 표준 라이브러리의 근간이다. `net/http`가 아무리 버전이 바뀌어도 이 인터페이스는 깨지지 않는다.

#### Middleware: 함수 래핑으로 직접 구현

향후 로깅, 인증 등 cross-cutting concern이 필요하다면:

```go
// middleware를 직접 구현하는 방식
func withLogging(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        start := time.Now()
        next.ServeHTTP(w, r)
        slog.Info("request", "method", r.Method, "path", r.URL.Path,
            "duration", time.Since(start))
    })
}

// 사용:
mux.Handle("POST /ingest", withLogging(httpinfra.NewHTTPHandler(svc)))
```

이 패턴은:
- 추가 의존성 없음
- 동작이 완전히 가시적
- `http.Handler`와 완벽 호환
- 컴파일러 인라인 가능 (단순한 래퍼 함수)

#### JSON Decode: encoding/json

```go
dec := json.NewDecoder(r.Body)
dec.DisallowUnknownFields()
if err := dec.Decode(&req); err != nil { ... }
```

`encoding/json`이 유일한 alloc 원인이다 (~512-byte internal buffer). 이것은 stdlib의 한계이며, 현재는 수용한다.

**만약 JSON decode가 실제 bottleneck으로 profiling에서 확인된다면:**
- `bytedance/sonic` (SIMD 가속 JSON): 동일 인터페이스, drop-in replacement
- `github.com/json-iterator/go`: reflection-free JSON
- 두 경우 모두, profiling 전에 교체하지 않는다. "빠를 것 같다"는 추측이 아니라 측정된 데이터가 교체 근거다.

---

## Performance Comparison

### net/http vs. Gin: 실제 차이는 어디서 발생하는가

**이론적 비교:**

```
동일 로직 기준 (라우트 1개, 미들웨어 없음):

net/http:
  http.ServeMux.ServeHTTP     : O(1) string match (라우트 2개)
  HTTPHandler.ServeHTTP       : 비즈니스 로직

Gin:
  gin.Engine.ServeHTTP        : radix tree lookup (~30ns for 100 routes)
  gin.Context.Next()          : middleware chain iteration
  yourHandler()               : 비즈니스 로직
  gin.Context.Reset()         : pool return reset
```

라우트 수가 2개인 경우 radix tree의 이점은 사실상 0이다. O(1) string match와 O(log n) radix tree에서 n=2일 때 차이는 noise level이다.

**측정 기준 (참조용 수치, 실제 측정 필요):**

| 시나리오 | 예상 처리 latency | 비고 |
|---|---|---|
| net/http + stdlib json | ~1–2 μs/req | JSON decode가 지배적 |
| Gin + ShouldBindJSON | ~1.5–2.5 μs/req | Context pool + binding overhead |
| Fiber + fasthttp | ~0.8–1.5 μs/req | net/http 우회, 생태계 비호환 |

**결론**: 라우트가 수십 개 이하인 경우 net/http와 Gin의 latency 차이는 **JSON decode 비용에 묻힌다**. JSON decode가 ~800 ns일 때 Gin의 routing/middleware overhead ~100 ns는 ~12%다. 이 12%를 위해 숨겨진 allocation, 프레임워크 내부 complexity, 외부 의존성을 감수하지 않는다.

### Allocation 비교

```
요청 처리 경로 allocation 분석:

[net/http 방식 — Core-X 현재]
  json.NewDecoder()           : 1 alloc (512-byte buffer)
  var req ingestRequest       : 0 alloc (stack)
  pool.Acquire()              : 0 alloc (pool hit)
  submitter.Submit()          : 0 alloc (channel send)
  ---
  합계: 1 alloc/req

[Gin 방식 — 가상]
  gin.Context (pool)          : 0 alloc (pooled, but Reset() cost)
  gin.Context.handlers slice  : 0 or 1 alloc (미들웨어 수에 따라)
  c.ShouldBindJSON            : 1 alloc (json.Decoder)
  yourHandler()               : 비즈니스 로직
  ---
  합계: 1–3 alloc/req (미들웨어 설정에 따라 다름)
```

`net/http`에서 allocation budget이 완전히 가시적이다. Gin에서는 Context internals, middleware chain dynamics를 알지 않으면 실제 alloc 수를 추측해야 한다.

---

## Consequences

### 성능 (DDIA: Reliability)

- **hot path allocation**: JSON decode 1회만 (stdlib 한계). 프레임워크 overhead 0.
- **GC pressure**: alloc rate 최소화 → GC trigger 빈도 감소 → STW pause 감소.
- **call stack depth**: 최소화. pprof에서 비즈니스 로직이 즉각 가시적.
- **latency 예측 가능성**: 프레임워크 내부 동작에 의한 unexpected latency spike 없음.

### 유지보수성 (DDIA: Maintainability)

**이득:**

- **외부 의존성 zero**: `go mod tidy` 실행 시 module graph가 단순하다. 프레임워크 버전 업그레이드, breaking change, 보안 취약점 대응이 불필요하다.
- **학습 가치**: `net/http`를 직접 다루면서 Go HTTP 서버의 실제 동작을 이해한다. `HandleFunc`, `ResponseWriter`, connection lifecycle, graceful shutdown — 이 모두가 Phase 2~4에서 직접 활용된다.
- **코드 가시성**: 성능 이슈가 발생했을 때 원인을 찾는 search space가 `net/http` + 비즈니스 코드로 한정된다.

**비용:**

- **코드량 증가**: 에러 처리, 타임아웃 설정, graceful shutdown을 직접 작성해야 한다.

```go
// Gin을 썼다면 한 줄:
router.Run(":8080")

// net/http에서는:
srv := &http.Server{
    Addr:         cfg.Addr,
    Handler:      mux,
    ReadTimeout:  5 * time.Second,
    WriteTimeout: 10 * time.Second,
    IdleTimeout:  60 * time.Second,
}
go srv.ListenAndServe()

// graceful shutdown:
ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()
srv.Shutdown(ctx)
```

이 추가 코드는 "boilerplate"가 아니다. **이 코드가 없으면 서버는 ReadTimeout 없이 동작하고, graceful shutdown이 없어서 in-flight 요청이 유실된다.** Gin을 썼어도 이 설정은 동일하게 필요하다 — 다만 `router.Run()` 뒤에 숨어서 보이지 않을 뿐이다.

- **라우팅 기능 없음**: Path parameter (`/events/:id`), group routing, named routes가 필요하다면 직접 구현하거나 `chi` 같은 lightweight router를 도입해야 한다.

Phase 1에서 필요하지 않다. Phase 2~3에서 필요해지면 그때 결정한다. YAGNI.

### 확장성 (DDIA: Scalability)

외부 의존성이 없으므로:
- `go build`가 network access 없이 동작한다 (CI/CD 신뢰성 향상).
- binary size가 최소화된다 (컨테이너 이미지 크기 감소).
- Go 버전 업그레이드 시 프레임워크 호환성 확인 불필요.

---

## Alternatives Considered

### 1. Gin 선택 + profiling으로 overhead 회피

**고려한 이유:**

"Gin을 써도 profiling으로 bottleneck을 찾아서 최적화하면 된다. 개발 속도가 더 빠르다."

**거부 이유:**

profiling이 Gin 내부의 allocation을 찾아낸다면, 해결책은 결국 "Gin의 해당 기능을 우회하거나 직접 구현"이다. 즉, Gin의 편의성을 포기하고 복잡성만 남는다. 처음부터 `net/http`로 시작하는 것이 더 단순하다.

또한 Core-X는 "Gin을 최적화하는 법을 배우는" 프로젝트가 아니다. Go 언어 자체에 대한 깊은 이해를 목표로 한다.

### 2. Echo 선택

**고려한 이유:**

Echo는 Gin보다 더 lightweight하고 net/http와 호환성이 좋다. middleware 체인이 더 명확하다는 평가도 있다.

**거부 이유:**

외부 의존성 도입이라는 근본적 문제는 동일하다. Echo의 장점은 Gin 대비 상대적 장점이지, `net/http` 대비 장점이 아니다. 라우트 2개, 미들웨어 없는 Core-X Phase 1에서 Echo가 `net/http`보다 나은 점이 없다.

### 3. Fiber (fasthttp 기반) 선택

**고려한 이유:**

벤치마크 수치가 가장 높다. `fasthttp`는 `net/http`의 allocation 패턴을 구조적으로 개선했다.

**거부 이유:**

앞서 Context에서 상세히 분석했다. 요약:
- `net/http` 생태계 비호환 (미들웨어, 도구, 테스팅 라이브러리 모두 재검토 필요)
- "빠름"이 Go 이해에서 오는 것이 아니라 프레임워크 교체에서 오는 것
- Phase 3에서 gRPC 추가 시 `net/http` 기반 코드와 `fasthttp` 기반 코드가 공존해야 하는 문제

### 4. Chi 선택

**고려한 이유:**

Chi는 `net/http` 호환이며, 의존성이 거의 없고, `http.Handler`를 그대로 사용한다. "net/http의 확장"에 가까운 포지셔닝이다.

**수용 조건:**

Chi는 Phase 1에서는 불필요하지만, Phase 2~3에서 라우트가 10개 이상으로 늘어나거나 path parameter가 필요해지면 도입을 재검토한다. Chi는 "프레임워크"가 아니라 "router"로, 이 ADR의 정신에 가장 가까운 대안이다.

---

## Related Decisions

- [ADR-002: Worker Pool & Sync.Pool 설계](0002-worker-pool-and-sync-pool-for-performance.md) — 이 결정으로 HTTP 레이어가 단순해졌기 때문에, worker pool과 sync.Pool에서 실질적인 성능 최적화가 가능해진다.
- [ADR-003: Clean Architecture + Zero-Allocation](0003-clean-architecture-with-zero-allocation.md) — "외부 의존 없음"이 Clean Architecture 적용을 단순하게 만든다. 프레임워크의 Context, Router 타입이 도메인 경계를 오염시키지 않는다.

---

## Monitoring / Validation

이 결정의 효과를 다음 지표로 검증한다.

| 지표 | 도구 | 목표값 | 확인 방법 |
|------|------|--------|-----------|
| Hot path alloc/op | `go test -bench=. -benchmem` | ≤ 1 alloc/op | BenchmarkIngest 실행 |
| go.mod 의존성 수 | `go mod graph` | 0 (non-stdlib) | CI에서 `grep -c "require" go.mod` |
| pprof call depth | `go tool pprof` | Handler 즉시 보임 | CPU profile에서 ServeHTTP 위치 확인 |
| Binary size | `go build -o core-x . && ls -lh core-x` | < 10 MB | build artifact 크기 |

```bash
# alloc 검증
go test ./internal/infrastructure/http/... -bench=BenchmarkIngest -benchmem -count=5

# 의존성 검증
go mod graph | wc -l  # 결과가 0 또는 직접 추가한 stdlib만

# binary 크기 검증
CGO_ENABLED=0 go build -ldflags="-s -w" -o /tmp/core-x ./cmd/main.go
ls -lh /tmp/core-x
```

**Phase 2 진입 전 재검토 조건:**

라우트가 10개를 초과하거나, path parameter가 필요해지거나, 공통 인증 미들웨어가 여러 라우트에 필요해지면 Chi 도입을 재검토한다. 단, 이 경우에도 Gin/Echo는 고려 대상이 아니다.
