# ADR-009: 읽기 경로 및 옵저버빌리티 (합의 알고리즘 이전)

## 상태
채택됨

## 배경

Phase 3b에서 비동기 WAL 스트리밍 레플리케이션을 완성했다. 시스템은 이벤트를 수집하고 복제할 수 있지만 두 가지 공백이 있다:
1. 읽기 API 없음 — `kv.Store.Get()`은 구현됐지만 `GET /kv/{key}`로 노출되지 않음
2. Prometheus 호환 메트릭 없음 — `/stats`는 JSON을 반환하지만 Prometheus 스크래핑 파이프라인에 연결 불가

ADR-007 Phase 4는 원래 Raft 합의 알고리즘을 계획했지만, Raft는 읽기 경로를 전제조건으로 요구한다 (GET 의미론 없이는 리더 선출의 효과를 관찰할 수 없다). 또한 Prometheus exporter 없이는 replLag와 ring 메트릭을 실시간으로 확인할 수 없다.

## 결정

### Phase 4가 Raft 이전에 구현하는 두 가지:

**1. 읽기 경로: `GET /kv/{key}`**
- 클러스터 라우팅은 쓰기 경로와 동일: `ring.Lookup(key)` → 로컬 처리 또는 gRPC 포워드
- 노드 간 포워딩을 위한 새로운 `KVService.Get` proto RPC
- Replica 노드는 KV 인덱스가 없음 (raw WAL bytes만 보유); 읽기는 ring lookup을 통해 Primary로 포워드
- Phase 5에서 Replica 로컬 읽기를 위한 인덱스 재구축 추가 예정

**2. Prometheus 옵저버빌리티**
- Application 계층에 `IngestMetrics` 포트 (application 코드에 Prometheus import 없음)
- `infrastructure/metrics` 패키지가 Prometheus로 포트를 구현
- HTTP 서버에 `/metrics` 엔드포인트 추가
- 레플리케이션 래그와 클러스터 ring 크기를 GaugeFunc으로 노출 (스크래핑 시점에 읽음)

### 왜 Raft가 아닌 읽기 경로 먼저인가?

- 읽기 경로 없이 Raft: 합의는 존재하지만 결과를 조회할 수 없음
- Raft 없이 읽기 경로: 즉시 동작하며 Prometheus로 관찰 가능
- 올바른 순서: 읽기 경로 → 옵저버빌리티 → Raft (Phase 5)

### 왜 커스텀 메트릭이 아닌 Prometheus인가?

- 업계 표준; 커스텀 도구 없이 Grafana, 알림 파이프라인에 연결 가능
- `client_golang`은 바이너리에 ~1MB 추가; gRPC 의존성이 이미 존재하므로 수용 가능
- ADR-001 예외 근거: 교육적 가치는 메트릭 설계에 있으며, Prometheus 클라이언트를 직접 구현하는 것이 목표가 아님

## 결과

**긍정적:**
- Core-X는 이제 단순한 수집 싱크가 아닌 완전한 읽기/쓰기 분산 KV 스토어가 됨
- replLag, 큐 포화도, ring 상태를 실시간으로 관찰 가능
- Prometheus로 Phase 3b 도구(부하 테스트, chaos 테스트)의 결과 분석 가능

**부정적:**
- Replica 읽기 경로: 단일 노드 모드에서 Replica 노드는 `GET /kv/{key}`에 404를 반환함 (인덱스 없음). 클러스터 모드에서는 ring이 읽기를 Primary로 자동 라우팅함
- Raft는 Phase 5로 연기; Primary 장애 시 여전히 수동 장애조치 필요

**모니터링 신호:**
- `core_x_replication_lag_bytes > 1MB` → Replica가 뒤처짐, Primary I/O 확인
- `core_x_worker_queue_depth / max_depth > 0.8` → 429 임박, 워커 스케일업 필요
