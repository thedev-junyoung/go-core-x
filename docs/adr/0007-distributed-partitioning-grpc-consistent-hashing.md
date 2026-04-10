# ADR-007: Distributed Partitioning — gRPC + Virtual Nodes Consistent Hashing

- **Status**: Accepted
- **Date**: 2026-04-10
- **Deciders**: Core-X Principal Architect

---

## Context

Phase 2까지 Core-X는 단일 노드 시스템이다. Bitcask 모델(ADR-005)은 in-memory hash index를 사용하므로 저장 가능한 키 수가 물리 메모리에 직접 바운드된다. 또한 단일 WAL writer는 fsync 속도에 의해 처리량이 상한된다.

Phase 3의 목표는 keyspace를 여러 노드에 분산하여 수평 확장 가능한 구조를 만드는 것이다.

### 요구사항

1. 각 source 키는 결정론적으로 특정 노드에 라우팅되어야 한다 (consistent routing)
2. 노드 추가/제거 시 최소한의 키 재배치만 발생해야 한다
3. 노드 간 통신은 고성능, 타입 안전한 프로토콜을 사용해야 한다
4. Application layer(IngestionService)는 무변경이어야 한다

### 핵심 결정 포인트

이 ADR은 세 가지 독립적인 결정을 포함한다:

1. **노드 간 통신 프로토콜**: 무엇으로 통신할 것인가
2. **파티셔닝 알고리즘**: 어떤 알고리즘으로 키를 노드에 배정할 것인가
3. **장애 시 fallback 정책**: 담당 노드 장애 시 어떻게 처리할 것인가

---

## Decision 1: 노드 간 통신 — gRPC + Protobuf

**ADR-001 확장**: ADR-001은 HTTP 프레임워크 미사용을 결정했다. 그 근거는 "편의성을 위한 추상화 비용"이었다. gRPC는 이와 다른 범주다.

| 항목 | ADR-001이 거부한 것 | gRPC |
|------|---------------------|------|
| 목적 | HTTP routing 편의성 | 노드 간 통신 프로토콜 |
| 대안 | net/http (동등 기능) | 직접 구현 시 HTTP/2 + 커스텀 직렬화 |
| 학습 가치 | 낮음 (라우터는 교육 대상이 아님) | 높음 (분산 시스템의 핵심 도구) |

gRPC를 직접 구현(HTTP/2 위에 바이너리 프레임 직렬화)하는 것은 Phase 3의 핵심 학습 목표(Consistent Hashing, Replication)를 달성하는 데 사용할 시간을 소비한다.

**결정**: `google.golang.org/grpc` + `google.golang.org/protobuf` 도입.
go.mod에 이 두 의존성만 추가된다. HTTP 프레임워크 금지 원칙은 유효하다.

### proto 스키마

```protobuf
syntax = "proto3";
package ingest;
option go_package = "github.com/junyoung/core-x/proto/pb";

service IngestionService {
  rpc Ingest(IngestRequest) returns (IngestResponse);
}

message IngestRequest {
  string source  = 1;
  bytes  payload = 2;
}

message IngestResponse {
  bool   ok      = 1;
  string message = 2;
}
```

---

## Decision 2: 파티셔닝 알고리즘 — Virtual Nodes Consistent Hashing

### 후보 분석

**Simple Consistent Hash (단일 포인트)**
- 각 물리 노드를 ring에 1개 포인트로 배치
- 장점: 구현 단순
- 단점: 노드 수가 적을 때 불균등 분산. 3개 노드 → 최대 편차 ~50%

**Jump Consistent Hash (Google, 2014)**
- 균등 분산 보장, 구현 10줄
- 단점: 노드 리스트가 append-only여야 함. 중간 노드 제거 시 알고리즘 가정이 깨짐.
  Core-X는 노드 제거가 필요한 운영 시나리오를 Phase 3에서 다룬다.

**Virtual Nodes (vnodes)**
- 각 물리 노드를 ring에 N개의 가상 포인트로 분산
- 균등 분산 + 노드 추가/제거 모두 지원
- Cassandra, DynamoDB가 실제로 사용하는 방식

### 결정: Virtual Nodes

```
vnodeCount = 150 (물리 노드당)

hash ring (sorted uint32):
  [12, 89, 203, 340, 512, 780, ...]
   N2   N1   N3   N2   N1   N3

Lookup("user-x"):
  h = fnv32a("user-x") = 300
  binary search → 340 → N2 담당
```

**vnodeCount = 150 근거**:
- Cassandra 기본값 (실전 검증된 수치)
- 3개 노드 기준 ring에 450 포인트 → 균등 분산 오차 < 5%
- 메모리 비용: 150 × 4 bytes × 노드수 = 무시 가능한 수준

**해시 함수**: FNV-32a
- CRC32보다 분산 균등성 우수
- SHA-256보다 연산 비용 낮음
- Go stdlib `hash/fnv` 사용 (외부 의존성 없음)

---

## Decision 3: 장애 시 Fallback 정책 — 에러 반환

### 후보 분석

**에러 반환 (선택)**
- 담당 노드 unhealthy → HTTP 503 반환
- 장점: 데이터 위치가 ring과 항상 일치. 숨겨진 불일치 없음.
- 단점: 가용성 저하 (담당 노드 장애 시 해당 키 쓰기 불가)

**Next-node fallback**
- ring에서 다음 노드로 자동 라우팅
- 장점: 일시적 가용성 향상
- 단점: "임시 저장 노드"와 "담당 노드" 불일치 상태 발생. 노드 복구 후 데이터 재조정 로직 필요. Phase 3 범위 초과.

**로컬 임시 저장 후 이전**
- 구현 복잡도 극단적으로 높음. Phase 3 범위 명확히 초과.

### 결정: 에러 반환

DDIA 원칙: "silent한 데이터 불일치보다 명시적 실패가 낫다."

Phase 3에서 Basic Replication이 구현되면, 담당 파티션의 replica가 자동으로 failover 역할을 할 수 있다. 에러 반환 정책은 그 구조로의 자연스러운 발전 경로다.

```
Phase 3a: 파티셔닝 (에러 반환)
Phase 3b: Basic Replication (replica가 fallback 역할)
Phase 4:  자동 failover (Raft 등 합의 알고리즘)
```

---

## Architecture

### Application Layer 무변경 원칙

Phase 3 전체는 Infrastructure layer 추가다.

```
기존 (Phase 2):
  HTTP Handler → IngestionService → kvStore

Phase 3 추가:
  HTTP Handler → [ring.Lookup] → 자신? → IngestionService → kvStore
                               → 타 노드? → gRPC Client → [타 노드의] IngestionService → kvStore
  gRPC Server → IngestionService → kvStore  (신규, 동일 Application layer 재사용)
```

`IngestionService`, `kvStore`, `WAL` — Phase 2에서 구현된 모든 코드는 무변경.

### 컴포넌트 구조

```
internal/infrastructure/
├── cluster/
│   ├── ring.go         # Virtual Nodes consistent hash ring
│   ├── node.go         # Node struct (ID, Addr, healthy atomic.Bool)
│   └── membership.go   # 노드 등록/제거, health probe goroutine
└── grpc/
    ├── server.go       # gRPC IngestionService 구현
    ├── client.go       # gRPC client pool (nodeID → *ClientConn)
    └── forward.go      # forward 결정 + gRPC 호출

proto/
└── ingest.proto        # 노드 간 통신 계약
```

---

## Consequences

### 성능

- **Forward latency**: LAN 기준 gRPC 왕복 ~0.5–2 ms. WAL write (~10 μs) 대비 100× 증가. 담당 노드가 아닌 경우에만 발생.
- **Local path**: 담당 노드로 직접 수신 시 Phase 2와 동일한 성능 (~10 μs).
- **ring.Lookup**: binary search O(log N×vnodeCount). 150 vnodes × 10 nodes = 1500 포인트. binary search ≈ 11 compare ≈ 10 ns.

### 신뢰성

- 담당 노드 장애 시 해당 source 키 쓰기 불가 (503 반환)
- 레플리케이션 없는 Phase 3a에서 노드 장애 = 해당 파티션 데이터 일시 불가
- Phase 3b (Replication) 구현 후 해소

### 유지보수성

- ADR-001의 "Pure Go" 원칙을 노드 간 통신 레이어에 한해 확장
- go.mod에 grpc, protobuf 두 의존성만 추가
- proto 스키마가 노드 간 계약의 single source of truth

### 확장성

- 노드 추가: `ring.AddNode()` 호출 → 기존 키의 일부만 재배치
- 노드 제거: `ring.RemoveNode()` 호출 → 해당 vnode들이 ring에서 제거
- 수평 확장: 노드 N배 → 처리량 N배 이론 상한 (각 노드가 1/N의 keyspace 담당)

---

## Monitoring / Validation

| 지표 | 도구 | 목표값 |
|------|------|--------|
| ring 균등 분산 | unit test: 1M key → 노드별 카운트 편차 | < 10% |
| forward latency p99 | 향후 Prometheus histogram | < 5 ms (LAN) |
| forward error rate | counter / 총 요청 | < 0.1% (정상 운영) |
| local path 성능 영향 | benchmark: ring.Lookup overhead | < 50 ns/op |

---

## Related Decisions

- [ADR-001: Pure Go 선택](0001-use-pure-go-standard-library.md) — gRPC 도입은 이 결정의 확장. HTTP 프레임워크 금지 원칙은 유효.
- [ADR-003: Clean Architecture](0003-clean-architecture-with-zero-allocation.md) — Application layer 무변경 원칙의 근거.
- [ADR-005: Hash Index KV Store (Bitcask)](0005-hash-index-kv-store-bitcask.md) — 파티셔닝의 대상이 되는 스토리지 레이어.
