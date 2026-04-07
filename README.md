# Project Core-X (High-Performance Event Engine)

> **"Build the Core, Lead the System."**
> DDIA와 Go 언어를 기반으로 구현하는 초고속 분산 이벤트 처리 엔진 프로젝트입니다.

## Project Vision
- **CEO/CTO Perspective**: 기술적 한계를 돌파하며 비즈니스 가치를 창출하는 아키텍처 구축.
- **High Performance**: 외부 프레임워크 최소화, Go 표준 라이브러리와 저수준 최적화 중심.
- **Reliability**: DDIA 철학에 기반한 데이터 유실 제로 시스템 지향.
- **AI-Driven**: 에이전트 코딩을 활용한 설계 리뷰 및 지능형 운영 자동화.

## Tech Stack
- **Language**: Go (Pure Go)
- **Communication**: gRPC, Protobuf
- **Storage**: Custom WAL (Write-Ahead Log), LSM-Tree Structure
- **Observability**: Prometheus, Elastic APM
- **Intelligence**: Claude / Cursor Agentic Workflow

---

## Architecture Layers

### Design Principle: Real Clean, Not Cosmetically Clean

계층 분리의 목적은 "폴더 구조가 깔끔해 보이는 것"이 아니라,
**변경이 전파되는 범위를 제한하는 것**입니다.

Phase 2에서 processor를 WAL 구현으로 교체할 때 HTTP handler가 영향받지 않아야 합니다.
Phase 3에서 gRPC 수집 엔드포인트를 추가할 때 비즈니스 규칙(validation)을 재작성하지 않아야 합니다.

### Dependency Rule

```
Infrastructure  →  Application  →  Domain
Infrastructure  →  Domain
(반대 방향 의존 금지)
```

### Layer Diagram

```
┌──────────────────────────────────────────────────────────────────┐
│  cmd/main.go  (Composition Root)                                 │
│  모든 concrete type을 알고 조립하는 유일한 장소.                     │
│  여기서의 import 방향 위반은 허용 — 이것이 wiring의 본질.            │
└────────────────────────┬─────────────────────────────────────────┘
                         │ constructs & wires
         ┌───────────────┼──────────────────────┐
         ▼               ▼                       ▼
┌────────────────┐ ┌─────────────────┐ ┌──────────────────┐
│ Infrastructure │ │   Application   │ │     Domain       │
│                │ │                 │ │                  │
│ http/          │ │ ingestion/      │ │ event.go         │
│   handler.go   │ │   service.go    │ │                  │
│   stats.go     │ │                 │ │ Event struct     │
│                │ │ IngestionService│ │ EventProcessor   │
│ pool/          │ │ Submitter port  │ │   interface      │
│   event_pool.go│ │ Stats port      │ │ EventProcessorFunc│
│                │ │ EventPool port  │ │                  │
│ executor/      │ │                 │ │ 외부 의존 없음.   │
│  worker_pool.go│ │                 │ │ stdlib time만.   │
└───────┬────────┘ └────────┬────────┘ └──────────────────┘
        │                   │                    ▲
        │   implements ports └────────────────── │
        └───────────────── depends on ──────────►│
```

### Directory Structure

```
core-x/
├── cmd/
│   └── main.go                         # Composition Root: 의존성 조립
│
└── internal/
    ├── domain/
    │   └── event.go                    # Event, EventProcessor, EventProcessorFunc
    │
    ├── application/
    │   └── ingestion/
    │       └── service.go              # IngestionService, Submitter/Stats/EventPool ports
    │
    └── infrastructure/
        ├── http/
        │   ├── handler.go              # HTTP wire format → IngestionService 변환
        │   └── stats.go                # /stats 엔드포인트
        ├── pool/
        │   └── event_pool.go           # sync.Pool 래퍼 (GC-aware object recycling)
        └── executor/
            └── worker_pool.go          # goroutine pool, implements Submitter/Stats
```

### File Responsibilities

| 파일 | 계층 | 책임 | 알고 있는 것 | 모르는 것 |
|---|---|---|---|---|
| `domain/event.go` | Domain | 핵심 비즈니스 타입 정의 | `Event` 구조, `EventProcessor` 계약 | HTTP, goroutine, sync.Pool |
| `application/ingestion/service.go` | Application | 이벤트 수집 유스케이스 조율 | 도메인 타입, 포트 인터페이스 | 채널, HTTP 상태 코드, pool 구현 |
| `infrastructure/http/handler.go` | Infrastructure | HTTP → 도메인 변환, 에러 → HTTP 상태 코드 변환 | `net/http`, JSON 디코딩, 와이어 포맷 | 워커풀 내부, pool 구현 |
| `infrastructure/http/stats.go` | Infrastructure | 엔진 카운터 노출 | `Stats` 포트, JSON 인코딩 | 카운터 구현 세부사항 |
| `infrastructure/pool/event_pool.go` | Infrastructure | Event 객체 재사용 (zero-alloc) | `sync.Pool`, `domain.Event.Reset()` | HTTP, 비즈니스 로직 |
| `infrastructure/executor/worker_pool.go` | Infrastructure | goroutine 실행 환경 제공 | 채널, goroutine, `domain.EventProcessor` | HTTP, 비즈니스 로직 |
| `cmd/main.go` | Wiring | 전체 시스템 조립 및 생명주기 관리 | 모든 concrete type | (의도적으로 모든 것을 알음) |

### Performance Guarantees by Layer

계층 분리가 성능을 희생하지 않는다는 근거:

- **Domain layer**: 인터페이스 1개 (`EventProcessor`). itab dispatch ~2ns/call. 호출 위치는 worker goroutine 내부 — HTTP accept path가 아님.
- **Application layer**: `IngestionService.Ingest()` 자체는 zero-allocation. pool.Acquire/Release + submitter.Submit 인터페이스 dispatch 2개 (~4ns 합산).
- **Infrastructure → Application 경계**: HTTP handler가 `IngestionService`를 concrete type으로 참조 (`*appingestion.IngestionService`). 인터페이스 dispatch 없음.
- **핵심 불변**: sync.Pool, buffered channel backpressure, atomic counter — Phase 1의 모든 zero-alloc 패턴이 그대로 유지됨.

### Extension Points by Phase

| Phase | 변경 위치 | 영향 범위 |
|---|---|---|
| Phase 2: WAL persistence | `executor/worker_pool.go`의 `Processor` 교체 (`domain.EventProcessor` 구현) | HTTP handler, Application service 무변경 |
| Phase 2: Binary serialization | `domain/event.go`에 `MarshalBinary()` 추가 | Infrastructure만 사용, Application 무관 |
| Phase 3: gRPC ingestion | `infrastructure/grpc/handler.go` 신규 추가 (동일 `IngestionService` 재사용) | Domain, Application 무변경 |
| Phase 3: Protobuf wire format | `infrastructure/http/handler.go`의 `ingestRequest` 교체 | Domain `Event` 타입 무변경 |
| Phase 4: Prometheus metrics | `infrastructure/http/stats.go` 교체 또는 병행 | `Stats` 포트 구현체만 변경 |

---

## Roadmap (Bottom-Up)

### Phase 1: High-Performance Heart (1.5mo)
- [x] No-Framework HTTP/TCP Collection Engine
- [x] Goroutine Worker Pool & Zero-copy logic
- [x] Clean Architecture Layering (zero-alloc preserved)
- [ ] Benchmarking & GC Optimization

### Phase 2: Trusted Persistence (2mo)
- [ ] Write-Ahead Log (WAL) implementation
- [ ] Key-Value Store with Hash Index
- [ ] Crash Recovery & Binary Serialization

### Phase 3: Distributed Scalability (2.5mo)
- [ ] Node-to-Node Communication (gRPC)
- [ ] Consistent Hashing for Partitioning
- [ ] Basic Consensus Algorithm for Replication

### Phase 4: Intelligence & Observability (Ongoing)
- [ ] Custom Metrics & Dashboard
- [ ] AI Agent Integration for Log Analysis
- [ ] Automated Fault Injection Testing

---

## Architecture Decision Records (ADR)

기록이 곧 실력입니다. 매 결정의 이유를 여기에 기록하세요.

- [ADR-001: Pure Go 선택](docs/adr/0001-use-pure-go-standard-library.md)
- [ADR-002: Worker Pool & Sync.Pool 설계](docs/adr/0002-worker-pool-and-sync-pool-for-performance.md)
- [ADR-003: Clean Architecture + Zero-Allocation](docs/adr/0003-clean-architecture-with-zero-allocation.md) ← Phase 1 완성

각 ADR은 설계 결정의 context, decision, consequences를 기록합니다.

---

## Project Owner (CEO/CTO)
- **Role**: Architecture Design, Code Review, Performance Monitoring
- **Daily Goal**: 1 Hour Deep Work
