# ADR-008: Basic Replication — Async WAL Streaming (Primary → Replica)

- **Status**: Accepted
- **Date**: 2026-04-11
- **Deciders**: Core-X Principal Architect

---

## Context

Phase 3a까지 Core-X는 일관된 해싱으로 keyspace를 파티셔닝했지만, 각 파티션은 단일 노드에만 저장된다. 노드 장애 시 해당 파티션은 완전히 불가(ADR-007 Decision 3: 에러 반환).

Phase 3b의 목표: **Primary → Replica 비동기 WAL 스트리밍**으로 단일 노드 장애 시에도 데이터를 보존한다.

### 요구사항

1. Primary WAL에 기록된 모든 Entry가 Replica WAL에 복제되어야 한다
2. Replica 장애 후 재시작 시 gap 없이 복구되어야 한다
3. Primary write path가 Replica 상태와 무관하게 계속 동작해야 한다 (async)
4. Application layer(IngestionService) 및 WAL Writer/Reader 코드는 무변경이어야 한다

### 핵심 결정 포인트

1. **역할 결정 방식**: 정적 vs 동적 leader election
2. **복제 프로토콜**: Sync vs Async, gRPC streaming 방식
3. **ACK 전략**: Per-entry ACK vs Reconnect-based offset
4. **Compaction 중 복제**: WAL atomic swap 시 streamer 핸들 처리

---

## Decision 1: 역할 결정 — 정적 환경 변수

**결정**: `CORE_X_ROLE=primary|replica` 환경 변수로 노드 역할을 정적으로 결정한다.

### 근거

Phase 3b의 목표는 "자동 failover"가 아닌 "durability 향상"이다. Raft/Paxos 기반 동적 leader election은:
- 구현 복잡도가 Phase 3b 범위를 크게 초과
- 학습 비용 대비 단계적 진화 경로가 더 명확함

```
Phase 3b: 정적 Primary/Replica (수동 절체)
Phase 4:  Raft 기반 자동 failover
```

역할은 Phase 3a의 ring에서 파티션 담당이 이미 결정되어 있으므로, 각 Primary 파티션에 Replica를 1:1 매핑한다.

```
환경 변수:
  CORE_X_ROLE=primary              # WAL 소유, streaming 발신
  CORE_X_ROLE=replica              # WAL 수신, local append
  CORE_X_PRIMARY_ADDR=host:port    # replica가 연결할 primary gRPC 주소
```

---

## Decision 2: 복제 프로토콜 — gRPC Server-side Streaming

**결정**: `ReplicationService.StreamWAL` — gRPC server-side streaming RPC.

### 후보 분석

| 방식 | 설명 | 결정 이유 |
|------|------|-----------|
| Unary RPC loop | 매 entry마다 독립 RPC | connection setup overhead, HOL blocking |
| Client streaming | Replica가 pull | Primary가 push 흐름 제어 불가 |
| **Server streaming** | Primary가 push | gRPC HTTP/2 flow control이 backpressure 처리 |
| Bidirectional streaming | 양방향 ACK | 구현 복잡도 불필요하게 높음 |

Server-side streaming은 Primary가 자연스러운 sender가 되고, gRPC의 HTTP/2 flow control이 Replica 소비 속도보다 빠른 전송을 자동으로 억제한다. 별도 backpressure 로직 불필요.

### WAL Tail-Follow 패턴

기존 `wal.Reader`는 `Scan() → EOF → 종료` 패턴이다. `ReplicationStreamer`는 EOF 후 polling loop로 전환한다.

```
offset = replica_request.start_offset

loop:
  record, err = ReadRawAt(walFile, offset)
  if found:
    stream.Send(WALEntry{offset, rawBytes, recordSize})
    offset += recordSize
  else (EOF):
    sleep(10ms)   ← 최대 복제 lag 상한 10ms
    continue
```

**polling 10ms 근거**: inotify/kqueue 대신 polling을 선택한 이유는 크로스플랫폼 구현 단순성과 O(0) 복잡도 유지. p99 replication lag target < 50ms 기준에서 10ms poll은 충분한 여유를 제공한다.

### Raw Bytes 전송

WAL entry를 decode → re-encode하지 않고 raw bytes를 그대로 전송한다.

- Primary의 CRC32가 Replica WAL에 그대로 보존됨 (checksum chain 유지)
- decode/re-encode 경로 없음 → allocation 없음
- Replica WAL이 Primary와 binary-identical → crash recovery 로직 재사용

---

## Decision 3: ACK 전략 — Reconnect-based Offset (PostgreSQL WAL receiver 모델)

**결정**: Explicit per-entry ACK 없이, Replica가 재연결 시 `start_offset`으로 마지막 fsync된 offset을 전달한다.

### 근거

Per-entry ACK는 왕복 latency를 복제 처리량의 병목으로 만든다. LAN 1ms RTT 기준 1000 ACK/s = 처리량 상한. Reconnect-based는 in-flight window가 gRPC send buffer 크기로 결정되어 훨씬 높은 처리량을 제공한다.

```
동작 흐름:
  1. Replica: persistedOffset = local WAL 파일 크기 (마지막 fsync된 offset)
  2. Replica → Primary: StreamWAL(start_offset = persistedOffset)
  3. Primary: ReadRawAt(persistedOffset) 부터 streaming
  4. Replica: WALEntry.raw_data를 local WAL에 append + fsync
  5. Replica: persistedOffset 업데이트 (in-memory)
  6. 연결 끊김 → goto 1 (persistedOffset = 파일 크기로 재계산)
```

**Durability guarantee**: Replica의 `wal.Writer.Write(rawData)` 후 `Sync()` 강제 호출. fsync된 것만 persistedOffset으로 인정. Replica crash 후 재시작 시 파일 크기로 persistedOffset 재계산 → gap 없는 복구.

---

## Decision 4: Compaction 중 Replication

WAL Compaction(`RunExclusiveSwap`)은 WAL 파일을 새 파일로 atomic rename한다. `ReplicationStreamer`의 `walFile` 핸들은 old inode를 가리키게 되어 compaction 이후 새 레코드를 놓치는 버그가 발생한다.

**결정**: `Writer`에 `CompactionNotify() <-chan struct{}` 메서드를 추가한다. `RunExclusiveSwap` 완료 후 channel을 새로 교체하여 신호를 보낸다. `ReplicationStreamer`는 이 신호를 받으면 WAL 파일을 재오픈하고 offset을 재설정한다.

```
streamer loop:
  select:
    case <-compactionCh:   // compaction 완료 신호
      walFile.Close()
      walFile = os.Open(walPath)
      offset = 0           // 새 compacted 파일의 시작
    case <-ctx.Done():
      return
    default:
      // 일반 read path
```

**왜 offset = 0인가**: Compaction은 "현재 시점의 유효한 데이터 전체"를 새 파일에 기록한다. Replica는 이 시점에서 새 파일 전체를 재수신해야 최신 상태를 보장받는다. 이것은 PostgreSQL의 `pg_basebackup` 이후 WAL streaming 재시작과 동일한 패턴이다.

---

## Architecture

### Application Layer 무변경 원칙

Phase 3b 전체는 Infrastructure layer 추가다.

```
기존 (Phase 3a):
  HTTP Handler → [ring.Lookup] → IngestionService → kvStore

Phase 3b 추가 (Primary):
  HTTP Handler → [ring.Lookup] → IngestionService → kvStore
                                                        ↓ (async, WAL 기록됨)
                              ReplicationStreamer polls WAL → gRPC stream → Replica

Phase 3b 추가 (Replica):
  ReplicationReceiver ← gRPC stream ← Primary
        ↓
  local WAL append + fsync
```

`IngestionService`, `kv.Store`, `wal.Writer`, `wal.Reader` — 무변경.

### 신규 컴포넌트

```
proto/ingest.proto              ← ReplicationService + StreamWAL RPC 추가

internal/infrastructure/
├── cluster/
│   └── role.go                 ← Role(Primary/Replica) enum + env parsing
└── replication/
    ├── streamer.go             ← Primary: WAL tail-follow → gRPC push
    ├── receiver.go             ← Replica: WALEntry 수신 → local WAL append + fsync
    ├── server.go               ← gRPC ReplicationService 서버 구현
    ├── client.go               ← Replica → Primary 연결 + reconnect loop
    └── lag.go                  ← replication_lag_bytes 모니터링 (atomic int64)

cmd/main.go                     ← replication 컴포넌트 wiring
```

**수정 파일**: `proto/ingest.proto`, `internal/infrastructure/storage/wal/writer.go` (CompactionNotify 추가), `cmd/main.go`

---

## Consequences

### Sync vs Async Trade-off

| | Sync Replication | **Async Replication (선택)** |
|---|---|---|
| **Durability** | Primary + Replica 모두 fsync 후 ACK | Primary fsync만 보장 |
| **Write Latency** | Primary + RTT + Replica fsync | Primary만 (RTT 없음) |
| **Availability** | Replica 장애 시 write block | Replica 장애 시 write 계속 |
| **Data Loss on Primary Failure** | 0 | 최대 replication_lag_bytes만큼 손실 가능 |

**Async 선택 근거**: Core-X는 ingestion 엔진이므로 수집 처리량이 최우선. Sync replication은 LAN 1ms RTT 기준 write latency를 최소 1ms 증가시키며, Replica 장애가 전체 write path를 블로킹한다.

**trade-off가 깨지는 조건**:
- `replication_lag_bytes` 지속적으로 > 10MB: 네트워크/디스크 병목 → Sync 재검토
- Primary MTTR > 5분: async 내구성 가정 깨짐 → Raft 도입 고려

### DDIA 3원칙

| 원칙 | Phase 3b의 결정 |
|------|----------------|
| **Reliability** | Async WAL replication으로 단일 노드 장애 시 내구성 향상. Replica crash recovery는 기존 WAL Reader 재사용으로 검증된 경로. |
| **Scalability** | Replication goroutine 1개/replica. Primary write path에 영향 없음. |
| **Maintainability** | Reconnect-based ACK: 별도 ACK 프로토콜 없음. Offset이 replication state의 단일 진실. 기존 WAL 코드 재사용. |

### 장애 시나리오

**Replica 장애**: Primary write 무중단. `/stats`에서 `replication_lag_bytes` 급증 → 운영 alert. Replica 복구 후 자동 gap 채움.

**Primary 장애**: 자동 failover 없음 (Phase 4). 운영자 수동 절체 필요. Split-brain 메커니즘 자체 부재 (수동 promotion만 가능).

---

## Monitoring / Validation

| 지표 | 구현 | Alert threshold |
|------|------|-----------------|
| `replication_lag_bytes` | `lag.Bytes()` → `/stats` 노출 | > 10MB |
| `replica_connected` | streamer context alive 여부 | 0 = alert |
| `replica_reconnect_count` | atomic counter | > 5/min |

**검증 절차**:
1. Primary에 write 1000 events
2. Replica WAL 파일 크기 = Primary WAL 파일 크기 확인 (bytes-identical)
3. Primary kill → Replica WAL에 persistedOffset까지 데이터 보존 확인
4. Primary restart → Replica reconnect → 이후 write gap 없음 확인

---

## Related Decisions

- [ADR-003: Clean Architecture](0003-clean-architecture-with-zero-allocation.md) — Application layer 무변경 원칙
- [ADR-006: WAL Compaction](0006-wal-compaction.md) — Compaction 중 Replication 처리의 전제
- [ADR-007: Distributed Partitioning](0007-distributed-partitioning-grpc-consistent-hashing.md) — Primary/Replica 역할 결정의 토대 (파티션 ring)
