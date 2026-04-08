# Core-X 시퀀스 다이어그램

프로젝트의 전체 흐름도를 시퀀스 다이어그램으로 표현합니다.

---

## 1️⃣ HTTP 수집 요청 흐름 (Ingest Path)

```mermaid
sequenceDiagram
    participant Client
    participant HTTP Handler
    participant IngestionService
    participant EventPool
    participant WALWriter
    participant WorkerPool
    participant Response

    Client->>HTTP Handler: POST /ingest<br/>{source, payload}
    HTTP Handler->>HTTP Handler: JSON Decode & Validate

    HTTP Handler->>IngestionService: Ingest(source, payload)
    IngestionService->>EventPool: Acquire()
    EventPool-->>IngestionService: *Event (pooled or new)

    IngestionService->>IngestionService: Set timestamp & data

    IngestionService->>WALWriter: WriteEvent(e)
    WALWriter-->>IngestionService: nil (or error)

    IngestionService->>WorkerPool: Submit(e)
    WorkerPool-->>IngestionService: true (or false if saturated)

    alt Submit Success
        IngestionService-->>HTTP Handler: nil
        HTTP Handler-->>Client: 202 Accepted
    else Pool Saturated
        IngestionService->>EventPool: Release(e)
        IngestionService-->>HTTP Handler: ErrOverloaded
        HTTP Handler-->>Client: 429 Too Many Requests
    else WAL Write Failed
        IngestionService->>EventPool: Release(e)
        IngestionService-->>HTTP Handler: error
        HTTP Handler-->>Client: 500 Internal Error
    end
```

---

## 2️⃣ 백그라운드 이벤트 처리 (Worker Pool)

```mermaid
sequenceDiagram
    participant WorkerPool as WorkerPool<br/>(jobCh)
    participant Worker1 as Worker 1
    participant Worker2 as Worker 2
    participant WorkerN as Worker N
    participant Processor
    participant EventPool
    participant Stats

    Note over WorkerPool: Buffered channel<br/>(capacity: NumWorkers × 10)

    WorkerPool->>Worker1: send Event
    WorkerPool->>Worker2: send Event
    alt Job queue full
        WorkerPool->>WorkerPool: return false (non-blocking)
    end

    par Worker Processing
        Worker1->>Processor: Process(e)
        Processor->>Processor: Handle event<br/>(log, aggregate, etc.)
        Worker1->>Stats: processedCount.Add(1)
        Worker1->>EventPool: Release(e)

        Worker2->>Processor: Process(e)
        Processor->>Processor: Handle event
        Worker2->>Stats: processedCount.Add(1)
        Worker2->>EventPool: Release(e)

        WorkerN->>Processor: Process(e)
        Processor->>Processor: Handle event
        WorkerN->>Stats: processedCount.Add(1)
        WorkerN->>EventPool: Release(e)
    end

    Note over EventPool: Event recycled for<br/>next Acquire()
```

---

## 3️⃣ Stats & Monitoring 엔드포인트

```mermaid
sequenceDiagram
    participant Client
    participant HTTP Handler
    participant WorkerPool
    participant Response

    Client->>HTTP Handler: GET /stats

    HTTP Handler->>WorkerPool: ProcessedCount()
    WorkerPool-->>HTTP Handler: int64 (atomic load)

    HTTP Handler->>WorkerPool: QueueDepth()
    WorkerPool-->>HTTP Handler: len(jobCh) (snapshot)

    HTTP Handler->>HTTP Handler: Build JSON response
    HTTP Handler-->>Client: 200 OK<br/>{processed, queue_depth}
```

---

## 4️⃣ Graceful Shutdown (신호 → 종료)

```mermaid
sequenceDiagram
    participant OS
    participant Main
    participant HTTP Server
    participant WAL Writer
    participant Worker Pool
    participant Workers as Worker<br/>Goroutines

    OS->>Main: SIGINT / SIGTERM
    Main->>Main: Shutdown signal handler

    Main->>HTTP Server: Shutdown(ctx)
    HTTP Server-->>HTTP Server: Stop accepting new requests
    HTTP Server-->>Main: Wait for in-flight requests

    Note over Main: ✓ Step 1: No new Submit() calls

    Main->>WAL Writer: Close()
    WAL Writer->>WAL Writer: Flush memory buffer → fsync
    WAL Writer-->>Main: ✓ fsync complete

    Note over Main: ✓ Step 2: All events in WAL

    Main->>Worker Pool: Shutdown(ctx)
    Worker Pool->>Worker Pool: close(jobCh)

    Workers->>Workers: Drain remaining events
    par Draining
        Workers->>Workers: Process last batch
        Workers->>Workers: Release to pool
    end

    Workers-->>Worker Pool: exit
    Worker Pool-->>Main: ✓ All workers done

    Note over Main: ✓ Step 3: Complete shutdown
    Main->>Main: Exit cleanly
```

---

## 5️⃣ 메모리 풀 재활용 (sync.Pool Lifecycle)

```mermaid
sequenceDiagram
    participant IngestionService
    participant EventPool as Pool
    participant SyncPool as sync.Pool
    participant GC

    IngestionService->>EventPool: Acquire()
    alt Pool Hit
        EventPool->>SyncPool: Get()
        SyncPool->>SyncPool: Return cached *Event
        SyncPool-->>EventPool: *Event (reused)
    else Pool Miss / GC
        SyncPool->>SyncPool: New() → allocate
        SyncPool-->>EventPool: *Event (new allocation)
    end
    EventPool-->>IngestionService: *Event (Reset state)

    IngestionService->>IngestionService: Use Event

    IngestionService->>EventPool: Release(e)
    EventPool->>EventPool: Reset() (clear fields)
    EventPool->>SyncPool: Put(e)

    Note over SyncPool: Cached for next Acquire()

    alt GC Runs
        GC->>SyncPool: Clear pool
        Note over EventPool: Next Acquire() triggers<br/>New() allocation
    else Sustained Load
        Note over EventPool: ~100% pool hit rate
    end
```

---

## 📊 주요 특징 요약

| 단계 | 책임 | 특징 |
|------|------|------|
| **HTTP Handler** | 요청 파싱 + 유효성 검사 | JSON 디코드, 필수 필드 검증 |
| **IngestionService** | 유스케이스 오케스트레이션 | Event 풀 관리, WAL 기록, Submit |
| **WorkerPool** | 비동기 처리 분산 | Non-blocking Submit, atomic 카운터 |
| **EventPool** | 메모리 재활용 | sync.Pool 기반, GC-aware |
| **WALWriter** | 내구성 보장 | Write-Ahead Log, fsync 정책 |
| **Graceful Shutdown** | 안전 종료 | 순서 보장 (HTTP → WAL → Workers) |

---

## 🏗️ 계층별 책임

### Domain Layer (`internal/domain`)
- **Event**: 수집 파이프라인의 데이터 단위
- **EventProcessor**: 이벤트 처리 인터페이스

### Application Layer (`internal/application/ingestion`)
- **IngestionService**: Ingest 유스케이스 오케스트레이션
- 포트 인터페이스: `Submitter`, `Stats`, `EventPool`, `WALWriter`

### Infrastructure Layer (`internal/infrastructure`)
- **HTTP Handler**: POST /ingest, GET /stats, GET /healthz
- **WorkerPool**: 비동기 워커 풀 (채널 기반)
- **EventPool**: sync.Pool 기반 메모리 풀
- **WALWriter**: Write-Ahead Log 디스크 저장

### Composition Root (`cmd/main.go`)
- 모든 의존성 생성 및 연결
- Graceful shutdown 순서 조율
