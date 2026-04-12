# Phase 4: Read Path + Observability - Architecture Guide

Core-X Phase 4 "읽기 경로 및 Prometheus 옵저버빌리티" 구현 문서입니다.

---

## 1. 왜 Raft보다 읽기 경로가 먼저인가

Phase 4 이전 상태: 이벤트를 수집하고 복제할 수 있지만 **관찰할 수 없었다**.

```
관찰 불가능한 시스템 → 디버깅 불가능

- GET /kv/{key} 없음  → Raft 리더 선출이 됐는지 확인할 방법 없음
- /metrics 없음       → replication lag, 큐 포화도를 실시간으로 알 수 없음
```

ADR-009의 핵심 논리:

```
읽기 경로 없이 Raft: 합의는 존재하지만 결과를 조회할 수 없음
Raft 없이 읽기 경로: 즉시 동작하며 Prometheus로 관찰 가능
→ 올바른 순서: 읽기 경로 → 옵저버빌리티 → Raft (Phase 5)
```

---

## 2. 읽기 경로: GET /kv/{key}

### 2-1. 라우팅 로직

쓰기(`POST /ingest`)와 **정확히 동일한 consistent hash ring**을 사용한다.

```
GET /kv/{key}
      │
      ▼
ring.Lookup(key)
      │
      ├─ 내 노드가 소유 ──────────────► kv.Store.Get(key) → 200 OK
      │
      └─ 다른 노드가 소유 ───────────► gRPC ForwardGet(target, key) → 포워드 응답
```

같은 규칙을 쓰지 않으면 consistent hash로 기록한 데이터를 찾을 수 없다.

### 2-2. 클러스터 모드와 단일 노드 모드

| 모드 | ring | 동작 |
|------|------|------|
| 단일 노드 | nil | 항상 로컬 읽기 |
| 클러스터 (Primary) | 설정됨 | ring lookup → 로컬 또는 포워드 |
| 클러스터 (Replica) | 설정됨 | ring이 Primary로 자동 라우팅 (Replica는 KV 인덱스 없음) |

> Replica 노드는 raw WAL bytes만 보유한다. 단일 노드 모드 Replica에서 직접 `GET /kv/{key}`를 호출하면 404를 반환한다. Phase 5에서 Replica 인덱스 재구축 예정.

### 2-3. 신규 gRPC RPC

노드 간 포워딩을 위해 `KVService.Get` RPC가 추가됐다:

```protobuf
service KVService {
  rpc Get(GetRequest) returns (GetResponse);
}
```

---

## 3. Prometheus 옵저버빌리티

### 3-1. Ports & Adapters 구조

Application 계층은 Prometheus를 직접 알지 않는다.

```
internal/
├── application/ingestion/
│   └── metrics.go          ← 포트 (인터페이스만 정의)
└── infrastructure/metrics/
    ├── ingestion.go        ← Prometheus 구현체
    └── noop.go             ← 테스트용 No-op
```

`IngestMetrics` 포트:

```go
type IngestMetrics interface {
    RecordIngest(status string, latencySeconds float64)
    RecordQueueDepth(depth int)
}
```

`IngestionService`는 `SetMetrics()`로 주입받는다. Prometheus import는 `infrastructure/metrics` 패키지에만 존재한다.

### 3-2. Zero-cost abstraction — nil 체크 패턴

메트릭이 없는 환경(단일 노드, 테스트)에서 `time.Now()` 호출 자체를 생략한다:

```go
func (s *IngestionService) Ingest(...) {
    var start time.Time
    if s.metrics != nil {
        start = time.Now()   // 메트릭 있을 때만 타이머 시작
    }
    // ... 처리 로직
    if s.metrics != nil {
        s.metrics.RecordIngest("ok", time.Since(start).Seconds())
    }
}
```

### 3-3. GaugeFunc 패턴

Raft 상태(term, 리더 여부), replication lag, ring 크기는 `GaugeFunc`으로 노출한다:

```go
prometheus.NewGaugeFunc(prometheus.GaugeOpts{
    Name: "core_x_raft_term",
}, func() float64 {
    return float64(node.Term())
})
```

Prometheus scraper가 `/metrics`를 호출하는 순간 클로저가 실행된다. **별도 동기화 goroutine이 필요 없다.**

### 3-4. 노출되는 메트릭 목록

| 메트릭 | 타입 | 설명 |
|--------|------|------|
| `core_x_ingest_total` | Counter | 수집 요청 수 (status 레이블) |
| `core_x_ingest_duration_seconds` | Histogram | 수집 처리 시간 |
| `core_x_worker_queue_depth` | Gauge | 워커 큐 현재 깊이 |
| `core_x_replication_lag_bytes` | GaugeFunc | 복제 지연 바이트 |
| `core_x_ring_size` | GaugeFunc | consistent hash ring 노드 수 |
| `core_x_raft_term` | GaugeFunc | 현재 Raft term |
| `core_x_raft_is_leader` | GaugeFunc | 리더 여부 (1/0) |

### 3-5. 알림 예시

```yaml
# replication lag
- alert: ReplicationLagHigh
  expr: core_x_replication_lag_bytes > 1048576  # 1MB
  annotations:
    summary: "Replica가 뒤처짐 — Primary I/O 확인"

# 워커 큐 포화
- alert: WorkerQueueNearFull
  expr: core_x_worker_queue_depth / 100 > 0.8
  annotations:
    summary: "429 임박 — 워커 스케일업 필요"

# Raft 리더 없음
- alert: NoRaftLeader
  expr: sum(core_x_raft_is_leader) == 0
  for: 10s
  annotations:
    summary: "클러스터에 Raft 리더 없음"
```

---

## 4. 전체 읽기 흐름 시퀀스

```
Client
  │
  │  GET /kv/user-x
  ▼
HTTPHandler
  │
  │  ring.Lookup("user-x") → Node-2 소유
  ▼
KVForwarder
  │
  │  gRPC KVService.Get(target=Node-2, key="user-x")
  ▼
Node-2 HTTPHandler
  │
  │  ring.Lookup("user-x") → 나(Node-2)가 소유
  ▼
kv.Store.Get("user-x")
  │
  │  HashIndex 조회 → WAL offset 반환 → 디스크 읽기
  ▼
Event{ source, payload, timestamp }
  │
  ▼
HTTP 200 JSON 응답
```

---

## 5. Phase 5에서 바뀌는 것

| 항목 | Phase 4 | Phase 5 (예정) |
|------|---------|----------------|
| Replica 읽기 | 404 (인덱스 없음) | 로컬 인덱스 재구축 후 읽기 가능 |
| `core_x_raft_is_leader` | 항상 0 (Raft 없음) | 실제 선출 결과 반영 |
| 장애조치 | 수동 | Raft 자동 리더 선출 |
