# Phase 3: Distributed Partitioning Design

- **Date**: 2026-04-10
- **Status**: Approved
- **Deciders**: Core-X Principal Architect

---

## Problem Statement

Phase 2는 단일 노드 Bitcask 모델 + WAL + Compaction까지 완성됐다.
단일 노드의 한계:

1. **메모리 바운드**: `HashIndex.maxKeys`가 물리 메모리에 제한됨
2. **SPOF**: 노드 1개 장애 = 전체 시스템 다운
3. **처리량 상한**: 단일 WAL writer의 fsync 속도 ~100K ops/sec

Phase 3 목표: 여러 노드에 keyspace를 분산하여 수평 확장 가능한 구조를 만든다.

---

## Architecture Overview

```
Client
  │  HTTP POST /ingest {source, payload}
  ▼
수신 노드 (HTTP Handler)
  │
  ├─ ring.Lookup(source) == 자신?
  │     └─ YES → kvStore.WriteEvent()   [기존 Phase 2 경로 그대로]
  │
  └─ NO  → gRPC Forward → 담당 노드
                │  IngestionService.Ingest()  [동일 Application layer]
                │
                └─ 담당 노드 unhealthy?
                      └─ 503 Service Unavailable 반환
```

핵심 원칙: **Application layer는 무변경**. gRPC server가 HTTP handler와 동일한
`IngestionService.Ingest()`를 호출한다. Phase 3 전체가 Infrastructure layer 추가다.

---

## Components

### 1. proto/ingest.proto

노드 간 통신 계약을 Protobuf로 정의한다.

```protobuf
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

**결정 근거**: HTTP wire format(`ingestRequest` struct)과 동일한 필드. Application layer의
`IngestionService`가 받는 `(source string, payload []byte)` 시그니처와 1:1 대응.

---

### 2. internal/infrastructure/cluster/ring.go — Virtual Nodes Consistent Hash Ring

**알고리즘**: Virtual Nodes (vnodes)

- 각 물리 노드를 ring에 `vnodeCount`(기본 150)개의 가상 포인트로 분산
- ring은 `[]uint32` sorted slice + `[]Node` 매핑으로 구현
- `Lookup(key string) Node`: SHA-256(key) % 2^32 → binary search → 담당 노드

```
ring (sorted uint32 포인트):
  [12, 89, 203, 340, 512, 780, ...]
   │    │    │    │    │    │
  N2   N1   N3   N2   N1   N3    (vnodes → 물리 노드 매핑)

Lookup("user-x"):
  hash("user-x") = 300
  binary search → 340 → N2 담당
```

**vnodeCount = 150 선택 근거**: Cassandra 기본값. 노드 3개 기준 ring에 450 포인트.
균등 분산 오차 < 5% (시뮬레이션 기준).

**인터페이스**:
```go
type Ring struct { ... }

func NewRing(vnodeCount int) *Ring
func (r *Ring) AddNode(node Node)
func (r *Ring) RemoveNode(nodeID string)
func (r *Ring) Lookup(key string) (Node, bool)
```

---

### 3. internal/infrastructure/cluster/node.go — Node 상태 관리

```go
type Node struct {
    ID      string       // 노드 식별자 (예: "node-1")
    Addr    string       // gRPC 주소 (예: "localhost:9001")
    healthy atomic.Bool  // 헬스 상태
}
```

---

### 4. internal/infrastructure/cluster/membership.go — Health Probe

백그라운드 goroutine이 각 노드에 주기적 ping을 보내 `healthy` 상태를 갱신한다.

- probe 간격: 5초 (설정 가능)
- probe 방법: gRPC connectivity state check (Dial + GetState)
- 장애 판정: 연속 2회 실패

---

### 5. internal/infrastructure/grpc/server.go — gRPC 수집 서버

gRPC `IngestionService` 구현. HTTP handler와 동일한 `IngestionService.Ingest()` 호출.

```go
type GRPCIngestionServer struct {
    svc *appingestion.IngestionService
    pb.UnimplementedIngestionServiceServer
}

func (s *GRPCIngestionServer) Ingest(ctx context.Context, req *pb.IngestRequest) (*pb.IngestResponse, error) {
    if err := s.svc.Ingest(req.Source, req.Payload); err != nil {
        return &pb.IngestResponse{Ok: false, Message: err.Error()}, nil
    }
    return &pb.IngestResponse{Ok: true}, nil
}
```

---

### 6. internal/infrastructure/grpc/client.go — gRPC Client Pool

노드당 1개의 `*grpc.ClientConn` 유지. `sync.Map`으로 nodeID → conn 관리.
커넥션 재사용으로 Phase 1의 zero-alloc 철학 유지.

```go
type ClientPool struct {
    conns sync.Map  // nodeID → *grpc.ClientConn
}

func (p *ClientPool) Get(node cluster.Node) (pb.IngestionServiceClient, error)
func (p *ClientPool) Close(nodeID string)
```

---

### 7. internal/infrastructure/http/handler.go 수정 — Forward 로직 추가

기존 handler에 ring lookup + forward 로직 추가. 자신이 담당이면 기존 경로 그대로.

```go
func (h *HTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    // ... decode req (기존과 동일)

    target, ok := h.ring.Lookup(req.Source)
    if !ok || target.ID == h.selfID {
        // 기존 로컬 처리 경로
        h.svc.Ingest(req.Source, req.Payload)
        return
    }

    if !target.IsHealthy() {
        http.Error(w, "target node unavailable", http.StatusServiceUnavailable)
        return
    }

    // gRPC forward
    h.forwarder.Forward(r.Context(), target, req.Source, req.Payload)
}
```

---

## Data Flow

### Write (local)
```
HTTP POST /ingest
  → ring.Lookup(source) == self
  → IngestionService.Ingest(source, payload)
  → kvStore.WriteEvent(event)     [WAL append + index update]
  → workerPool.Submit(event)      [async processing]
```

### Write (forward)
```
HTTP POST /ingest
  → ring.Lookup(source) == Node B
  → Node B healthy? YES
  → grpc ClientPool.Get(Node B)
  → pb.IngestionServiceClient.Ingest(ctx, req)
  → [Node B] IngestionService.Ingest(source, payload)
  → [Node B] kvStore.WriteEvent(event)
```

### Write (forward, 장애)
```
HTTP POST /ingest
  → ring.Lookup(source) == Node B
  → Node B healthy? NO
  → HTTP 503 Service Unavailable
```

---

## Basic Replication (Phase 3 후반)

파티셔닝 구현 후 추가할 레이어:

```
Primary 노드 Write 완료
  └─ async gRPC → Replica 노드들 (ring에서 다음 N개)
       └─ WAL append only (in-memory index 미구축)
       └─ Primary 장애 시 수동 승격 (Phase 3 범위)
       └─ 자동 failover는 Phase 4 (Raft)
```

Replication factor = 2 (기본). 설정 가능.

---

## Error Handling

| 상황 | 동작 |
|------|------|
| 담당 노드 unhealthy | HTTP 503 반환 |
| gRPC forward timeout (3s) | HTTP 503 반환 |
| gRPC forward 성공 | HTTP 200 반환 |
| ring이 비어있음 | 로컬 처리 (단일 노드 모드) |

---

## Configuration

`cmd/main.go`에서 환경변수로 주입:

```
CORE_X_NODE_ID=node-1
CORE_X_GRPC_ADDR=:9001
CORE_X_PEERS=node-2:localhost:9002,node-3:localhost:9003
CORE_X_VNODE_COUNT=150
CORE_X_PROBE_INTERVAL=5s
CORE_X_FORWARD_TIMEOUT=3s
```

---

## Testing Strategy

| 테스트 | 방법 |
|--------|------|
| ring 균등 분산 | 1M key → 노드별 카운트, 편차 < 10% |
| ring vnode lookup | unit test, 경계값(ring 최대/최소) |
| forward 성공 | 2-노드 in-process gRPC (bufconn) |
| forward 장애 | 담당 노드 unhealthy 강제 설정 → 503 확인 |
| Application layer 무변경 | 기존 IngestionService 테스트 통과 확인 |

---

## Dependencies

```
google.golang.org/grpc
google.golang.org/protobuf
```

ADR-001 "Pure Go" 원칙의 공식 확장. 상세 근거는 ADR-007 참조.

---

## Metrics (Phase 4 연계)

```
ring_nodes_total              gauge     — ring에 등록된 노드 수
ring_forward_latency_p99      histogram — gRPC forward 왕복 시간
ring_forward_errors_total     counter   — forward 실패율 (노드별)
node_health_status            gauge     — 노드별 healthy 상태 (1/0)
```
