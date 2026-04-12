# Project Core-X (High-Performance Event Engine)

> **"Build the Core, Lead the System."**
> DDIA와 Go 언어를 기반으로 구현하는 초고속 분산 이벤트 처리 엔진 프로젝트입니다.

## Project Vision
- **CEO/CTO Perspective**: 기술적 한계를 돌파하며 비즈니스 가치를 창출하는 아키텍처 구축.
- **High Performance**: 외부 프레임워크 최소화, Go 표준 라이브러리와 저수준 최적화 중심.
- **Reliability**: DDIA 철학에 기반한 데이터 유실 제로 시스템 지향.
- **AI-Driven**: 에이전트 코딩을 활용한 설계 리뷰 및 지능형 운영 자동화.

## Tech Stack
- **Language**: Go
- **Communication**: gRPC (`google.golang.org/grpc`), Protobuf (노드 간 통신)
- **Storage**: Custom WAL (Write-Ahead Log), Hash Index KV Store (Bitcask model)
- **Partitioning**: Virtual Nodes Consistent Hashing (Phase 3)
- **Observability**: Prometheus, Elastic APM (Phase 4)
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
    │       └── service.go              # IngestionService, Submitter/Stats/WALWriter ports
    │
    └── infrastructure/
        ├── http/
        │   ├── handler.go              # HTTP wire format → IngestionService 변환
        │   └── stats.go                # /stats 엔드포인트
        ├── pool/
        │   └── event_pool.go           # sync.Pool 래퍼 (GC-aware object recycling)
        ├── executor/
        │   └── worker_pool.go          # goroutine pool, implements Submitter/Stats
        ├── cluster/                    # Phase 3: 분산 클러스터 멤버십
        │   ├── ring.go             # Virtual Nodes consistent hash ring
        │   ├── ring_test.go
        │   ├── node.go             # Node struct (ID, Addr, healthy atomic.Bool)
        │   └── membership.go       # 노드 health probe goroutine
        ├── raft/                       # Phase 5: Raft 합의 모듈
        │   ├── state.go            # RaftRole, 상수 (timeout, heartbeat interval)
        │   ├── node.go             # RaftNode 상태 머신 (§5.2, §5.3, §5.4)
        │   ├── node_test.go
        │   ├── server.go           # RaftHandler gRPC 서버
        │   ├── server_test.go
        │   ├── client.go           # RaftClient (RequestVote / AppendEntries gRPC 호출)
        │   └── meta_store.go       # MetaStore — raft_meta.bin 영속화 (CRC32)
        ├── grpc/                       # Phase 3: 노드 간 gRPC 통신
        │   ├── server.go           # gRPC IngestionService 구현
        │   ├── client.go           # gRPC client pool (nodeID → *ClientConn)
        │   └── forward.go          # forward 결정 + gRPC 호출
        └── storage/
            ├── wal/                    # Write-Ahead Log (Phase 2)
            │   ├── writer.go
            │   ├── reader.go
            │   ├── encode.go
            │   └── errors.go
            └── kv/                     # Hash Index KV Store (Phase 2, Bitcask model)
                ├── index.go            # in-memory hash index: map[string]int64
                ├── store.go            # KV store: WriteEvent/Get/Recover
                └── store_test.go

proto/
└── ingest.proto                        # Phase 3: 노드 간 통신 계약 (Protobuf)
    pb/
    ├── ingest.pb.go                # protoc-gen-go 생성
    └── ingest_grpc.pb.go           # protoc-gen-go-grpc 생성
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
| Phase 3: gRPC ingestion | `infrastructure/grpc/server.go` 신규 추가 (동일 `IngestionService` 재사용) | Domain, Application 무변경 |
| Phase 3: Consistent Hashing | `infrastructure/cluster/ring.go` 신규 추가, `http/handler.go`에 ring lookup 추가 | IngestionService 무변경 |
| Phase 4: Prometheus metrics | `infrastructure/http/stats.go` 교체 또는 병행 | `Stats` 포트 구현체만 변경 |

---

## Phase 2: Bitcask Model KV Store

### Write Path (Ingest)

```
HTTP POST /ingest {source, payload}
         ↓
IngestionService.Ingest(source, payload)
         ├─► pool.Acquire()  [0 alloc if hit]
         │         ↓
         │   Event{ReceivedAt, Source, Payload}
         │
         ├─► kvStore.WriteEvent(e)
         │         ├─► writer.WriteEventOffset(e)  [offset 반환]
         │         │        ├─ sizeTracker.Load()  [pre-write offset]
         │         │        ├─ file.Write(buf)    [WAL append]
         │         │        └─ sizeTracker.Add()   [누산]
         │         │
         │         └─► index.Set(e.Source, offset)  [in-memory map]
         │              └─ RLock 없음, Lock 획득 후 map[string] = int64
         │
         ├─► workerPool.Submit(e)  [비동기 처리]
         │         ├─ select { case jobCh <- e }
         │         └─ 채널 full이면 false → 429
         │
         └─ return nil/error

Performance:
  - WriteEvent: ~10 μs (SyncInterval policy)
  - Allocations: 0 (pool hit) or 1 (map grow on new key)
```

### Read Path (Get)

```
HTTP GET /get/{source}  (example, not in Phase 2 API yet)
         ↓
kvStore.Get(source)
         ├─► index.Get(source)  [RLock]
         │        ├─ mu.RLock()
         │        ├─ offset, found := entries[source]
         │        └─ return (offset, found)  [0 alloc]
         │
         ├─► ReadRecordAt(readFile, offset)
         │        ├─ file.ReadAt(header, offset)     [16 bytes]
         │        ├─ file.ReadAt(payload, offset+16)  [N bytes]
         │        ├─ file.ReadAt(checksum, offset+16+N)  [4 bytes]
         │        └─ CRC32 validation
         │
         ├─► wal.DecodeEvent(record.Data)  [parsing]
         │
         └─ return *Event

Performance:
  - index.Get: ~50 ns (RLock + map lookup)
  - ReadRecordAt: ~1-2 μs (single ReadAt syscall)
  - DecodeEvent: ~100-200 ns
  - Total: ~2-3 μs

Allocations: 2 (Event struct + string data, unavoidable for value return)
```

### Recovery Path (Startup)

```
cmd/main.go initialization:
         ↓
kvStore.Recover(onRecover)
         ├─► wal.NewReader(walPath)
         │
         ├─ for reader.Scan() {
         │      record := reader.Record()
         │      event := wal.DecodeEvent(record.Data)
         │
         │      ├─ index.Set(event.Source, offset)  [index 재구축]
         │      │
         │      └─ onRecover(event)  [콜백]
         │           └─ workerPool.Submit(event)  [처리 복구]
         │
         │      offset += RecordSize
         │   }
         │
         └─ return (count, error)

Recovery semantics:
  - WAL에 있는 모든 완전한 레코드 → index rebuild
  - ErrTruncated (마지막 불완전): 정상 처리
  - ErrCorrupted/ErrChecksumMismatch: fatal

Startup time: O(WAL size) ~1 μs/record
  예: 100M events → ~100 sec (worst case)
  Phase 3에서 compaction 도입 후 개선
```

### Crash Guarantees

```
Write 직전:           offset = 100
  ↓
Write OK:             file[100..163] = serialized Event
  ↓
index.Set():          index["user-a"] = 100
  ↓
Crash (any point):    Recovery replays WAL
  ├─ Crash during index.Set():
  │   → WAL에 데이터 있음
  │   → Recover에서 읽음
  │   → index 재구축
  │   (eventual consistency, ~nanoseconds drift)
  │
  └─ Crash before Write:
      → WAL에 데이터 없음
      → Recover에서 보이지 않음
      (원자성 보장)

Single source of truth: WAL
  → "Crash 직후 Get(source)" = "Crash 직전의 최신 값"
  → (Crash 직후 index 미구축 가능하지만, Recover로 자동 복구)
```

---

## Phase 3: Distributed Partitioning

### Cluster Configuration

환경 변수로 클러스터 모드를 활성화한다. 미설정 시 단일 노드 모드로 동작한다.

```bash
# Node 1
CORE_X_NODE_ID=node-1 \
CORE_X_GRPC_ADDR=:9001 \
CORE_X_PEERS="node-2:localhost:9002,node-3:localhost:9003" \
CORE_X_ADDR=:8081 \
./core-x

# Node 2
CORE_X_NODE_ID=node-2 \
CORE_X_GRPC_ADDR=:9002 \
CORE_X_PEERS="node-1:localhost:9001,node-3:localhost:9003" \
CORE_X_ADDR=:8082 \
./core-x
```

### Request Routing

```
POST /ingest {source: "user-x", payload: "..."}
  │
  ▼ ring.Lookup("user-x") → node-2 담당
  │
  ├─ 수신 노드 == node-2? → 로컬 kvStore.WriteEvent()
  └─ 수신 노드 != node-2? → gRPC forward → node-2
                                └─ node-2 unhealthy? → HTTP 503
```

### Phase 5b 운영 참고: raft_meta.bin

Raft가 활성화된 클러스터에서 각 노드는 작업 디렉토리에 `raft_meta.bin` (269 bytes)을 생성한다.
이 파일은 `currentTerm`과 `votedFor`를 영속화하며, 재시작 시 O(1)로 복구된다.

- **파일이 없으면**: `term=0`, `votedFor=""` 초기 상태로 시작 (정상)
- **백업·복구 시**: WAL(`wal.log`)과 함께 반드시 포함해야 한다
- **CRC32 불일치 감지 시**: 노드가 시작을 거부하고 에러를 로깅한다

### Virtual Nodes Ring

- 노드당 150 vnodes (기본값, `CORE_X_VNODE_COUNT`로 조정 가능)
- FNV-32a 해시, binary search로 담당 노드 결정 (~10 ns)
- 3개 노드 기준 균등 분산 오차 < 5%

---

## Roadmap (Bottom-Up)

### Phase 1: High-Performance Heart ✅ COMPLETE
- [x] No-Framework HTTP/TCP Collection Engine
- [x] Goroutine Worker Pool & Zero-copy logic
- [x] Clean Architecture Layering (zero-alloc preserved)
- [x] Benchmarking & GC Optimization

### Phase 2: Trusted Persistence ✅ COMPLETE (2026-04-09)
- [x] Write-Ahead Log (WAL) implementation
- [x] Key-Value Store with Hash Index (Bitcask model)
- [x] Crash Recovery with WAL replay
- [x] WAL Compaction (stop-the-world, atomic rename)

### Phase 3: Distributed Scalability ✅ COMPLETE
- [x] Node-to-Node Communication (gRPC) — `infrastructure/grpc/`
- [x] Consistent Hashing for Partitioning — Virtual Nodes, `infrastructure/cluster/`
- [x] Basic Replication — Primary→Replica async WAL streaming — `infrastructure/replication/`

### Phase 4: Intelligence & Observability ✅ COMPLETE (2026-04-11)
- [x] Read Path — `GET /kv/{key}` with ring-aware routing
- [x] Prometheus Metrics — ingest/replication/cluster 게이지 + 카운터
- [x] `/metrics` 엔드포인트 (Prometheus scrape)

### Phase 5a: Raft Leader Election ✅ COMPLETE (2026-04-11)
- [x] Raft 상태 머신 — Follower / Candidate / Leader
- [x] RequestVote + AppendEntries(heartbeat) gRPC RPC
- [x] 자동 리더 선출 (election timeout 150–300ms, randomized)
- [x] Prometheus 메트릭 — `core_x_raft_term`, `core_x_raft_is_leader`
- [x] 클러스터 모드에서 RaftNode 자동 시작

### Phase 5b: Raft Log Replication ✅ COMPLETE (2026-04-12)
- [x] MetaStore — `raft_meta.bin` 영속화 (`currentTerm`, `votedFor`, CRC32 무결성)
- [x] §5.3 Log Matching — `AppendEntries` proto 확장, `prevLogIndex/prevLogTerm` 검증
- [x] Fast Backup — `conflictIndex/conflictTerm` 기반 O(distinct terms) nextIndex 수렴
- [x] `commitIndex` 추적 + §5.4.2 Current Term Only Commit
- [x] RoleController — 50ms polling으로 Raft 역할 변화를 ReplicationManager에 전달
- [x] ManagedReplicationServer — atomic flag null-object 패턴으로 gRPC 역할 전환
- [x] `cmd/main.go` 정적 `CORE_X_ROLE` 제거 → RoleController 자동 전환

### Phase 5c: WAL-Backed Log Persistence ✅ COMPLETE (2026-04-12)
- [x] `LogStore` 인터페이스 — `Append`, `TruncateSuffix`, `LoadAll`, `Close`
- [x] `WALLogStore` — 기존 `wal.Writer`/`wal.Reader` 재사용, `SyncImmediate` 정책
- [x] `MemLogStore` — 테스트용 인메모리 구현
- [x] `HandleAppendEntries` persist-before-update (truncation + append)
- [x] `NewRaftNode` startup 시 `LoadAll()`로 로그 복구
- [x] `cmd/main.go` `WALLogStore` 주입 (`data/raft_log.wal`)
- [x] ADR-012: Raft 로그 WAL 영속화 설계 근거

### Phase 5d: Leader Write Path ✅ COMPLETE (2026-04-12)
- [x] `Propose(data []byte) (index, term int64, isLeader bool)` — leader log append + persist
- [x] `ApplyCh() <-chan LogEntry` — 커밋된 엔트리 전달 채널
- [x] `runApplyLoop` — 5ms 폴링, `commitIndex > lastApplied` 시 `applyCh`로 전달
- [x] 단일 노드 fast-path — heartbeat tick마다 즉시 `maybeAdvanceCommitIndex`
- [x] ADR-013: Propose + Apply channel 설계 근거

### Phase 6: Raft KV State Machine + HTTP 연동 ✅ COMPLETE (2026-04-12)
- [x] `KVStateMachine` — `ApplyCh()` 소비, in-memory map에 set/del 반영
- [x] `WaitForIndex(ctx, index)` — 동기 apply-wait (선형적 읽기-후-쓰기 보장)
- [x] `POST /raft/kv` — JSON body `{op, key, value}` → Propose → 204 No Content
- [x] `GET /raft/kv/{key}` — Raft 상태 머신에서 직접 읽기
- [x] 리더가 아닐 때 503 반환 (클라이언트 재시도/redirect 처리)
- [x] ADR-014: KV 상태 머신 + HTTP 연동 설계 근거

### Phase 7: 멀티 노드 통합 테스트 + 리더 Redirect ✅ COMPLETE (2026-04-12)
- [x] `RaftNode.LeaderID()` — 현재 알려진 리더 ID 반환 (팔로워가 heartbeat에서 학습)
- [x] `ProposeHandler` — 팔로워 요청 시 307 Temporary Redirect → 리더 HTTP 주소
- [x] `CORE_X_RAFT_HTTP_NODES` 환경 변수 — nodeID→HTTP base URL 매핑
- [x] `cluster_test.go` — 실제 loopback gRPC 3-노드 클러스터 통합 테스트
  - `TestCluster_ElectsLeader` — 5초 내 단일 리더 선출 검증
  - `TestCluster_ProposeAndReplicate` — 리더 Propose → 전체 노드 상태 머신 반영
  - `TestCluster_LeaderIDKnownToFollowers` — heartbeat 후 팔로워의 LeaderID() 검증
- [x] ADR-015: 리더 Redirect + 멀티 노드 통합 테스트 설계 근거

---

## Architecture Decision Records (ADR)

기록이 곧 실력입니다. 매 결정의 이유를 여기에 기록하세요.

### Phase 1 (Complete)
- [ADR-001: Pure Go 선택](docs/adr/0001-use-pure-go-standard-library.md)
- [ADR-002: Worker Pool & Sync.Pool 설계](docs/adr/0002-worker-pool-and-sync-pool-for-performance.md)
- [ADR-003: Clean Architecture + Zero-Allocation](docs/adr/0003-clean-architecture-with-zero-allocation.md)
- [ADR-004: WAL Reader 설계](docs/adr/0004-wal-reader-design.md)

### Phase 2 (Complete)
- [ADR-005: Hash Index KV Store (Bitcask Model)](docs/adr/0005-hash-index-kv-store-bitcask.md)
- [ADR-006: WAL Compaction](docs/adr/0006-wal-compaction.md) ← Phase 2 완성

### Phase 3 (Complete)
- [ADR-007: Distributed Partitioning — gRPC + Virtual Nodes Consistent Hashing](docs/adr/0007-distributed-partitioning-grpc-consistent-hashing.md)
- [ADR-008: Basic Replication — Async WAL Streaming](docs/adr/0008-basic-replication-async-wal-streaming.md)

### Phase 4 (Complete)
- [ADR-009: Read Path & Observability](docs/adr/0009-read-path-and-observability.md)

### Phase 5a (Complete)
- [ADR-010: Raft 리더 선출](docs/adr/0010-raft-leader-election.md)

### Phase 5b (Complete)
- [ADR-011: Raft 메타데이터 영속화, §5.3 Log Replication, 역할 전환](docs/adr/011-raft-log-persistence-and-role-transition.md)

### Phase 5c (Complete)
- [ADR-012: Raft 로그 WAL 영속화](docs/adr/012-raft-log-wal-persistence.md)

### Phase 5d (Complete)
- [ADR-013: Raft Write Path — Propose + Apply Channel](docs/adr/013-raft-write-path.md)

### Phase 6 (Complete)
- [ADR-014: Raft KV State Machine — HTTP Propose + Apply-Wait](docs/adr/014-raft-kv-state-machine.md)

### Phase 7 (Complete)
- [ADR-015: Raft Leader Redirect + 멀티 노드 통합 테스트](docs/adr/015-raft-leader-redirect-and-multinode-test.md)

각 ADR은 설계 결정의 context, decision, consequences를 기록합니다.

---

## Project Owner (CEO/CTO)
- **Role**: Architecture Design, Code Review, Performance Monitoring
- **Current Status**: Phase 7 Complete (2026-04-12)
- **Completed**: Phase 5a/5b/5c/5d/6/7 — Leader Election, Log Replication, Log Persistence, Write Path, KV State Machine HTTP, Leader Redirect + Multi-node Tests
- **Next**: Phase 8 — Raft 스냅샷(log compaction) / 멤버십 변경
