# Load Test & Chaos Engineering Guide

Core-X의 부하 테스트와 장애 주입 테스트를 실행하는 방법과 실제 측정 결과를 정리한 문서입니다.

---

## Core-X Phase 개요

각 Phase는 이전 Phase 위에 쌓이는 구조입니다. 테스트는 항상 이 맥락을 염두에 두고 설계되었습니다.

| Phase | 핵심 컴포넌트 | 추가된 것 | 관련 코드 |
|-------|--------------|-----------|-----------|
| **Phase 1** | In-Memory Store | Worker pool, sync.Pool, zero-allocation HTTP 파이프라인 | `internal/application/ingestion/` |
| **Phase 2** | WAL + Hash Index KV (Bitcask) | 영속성 — WAL append-only 기록 + 메모리 hash index로 O(1) lookup | `internal/infrastructure/storage/wal/`, `storage/kv/` |
| **Phase 3** | WAL Compaction | Stop-the-world 컴팩션 — 오래된 WAL 엔트리 병합, 디스크 공간 회수 | `storage/kv/compact.go` |
| **Phase 4** | Consistent Hashing Ring + gRPC | 수평 확장 — virtual node 기반 keyspace 파티셔닝, 노드 간 gRPC 포워딩, Health Probe, 503 Fail-fast | `internal/infrastructure/cluster/ring.go`, `cluster/membership.go` |

### 각 Phase가 무엇을 "증명"하는가

- **Phase 1**: 순수 메모리 처리 한계. Go runtime scheduler + sync.Pool의 극한 처리량.
- **Phase 2**: WAL fsync 오버헤드 vs 내구성 트레이드오프. Bitcask 모델의 append-only 특성상 쓰기가 순차적이므로 Phase 1 대비 I/O 레이턴시 증가폭이 작아야 한다.
- **Phase 3**: 컴팩션이 진행 중일 때 stop-the-world가 p99 레이턴시에 얼마나 영향을 주는지.
- **Phase 4**: gRPC 포워딩 오버헤드(노드 간 네트워크 홉)와 장애 격리(Cascading Failure 방지) 동작 검증.

---

## 디렉터리 구조

```
tools/
├── loadgen/          # Load Generator 라이브러리
│   ├── generator.go  # Fixed worker pool + rate-limiter (closed-loop / open-loop)
│   ├── histogram.go  # Lock-free 레이턴시 히스토그램 (atomic.Int64, power-of-2 bucket)
│   └── report.go     # Phase별 비교 리포트 출력
├── chaos/
│   ├── cluster.go    # OS 프로세스 기반 3노드 클러스터 관리 + kill -9 주입
│   └── chaos_test.go # Phase 4 장애 시나리오 테스트
└── benchmark/
    └── main.go       # CLI 벤치마크 러너 (warmup 포함, CI용 exit code)
scripts/
└── bench_phases.sh   # Phase 1~4 자동 비교 스크립트
```

---

## 실행 방법

### 1. Load Generator 단위 테스트

Phase 1~4 공통 — load generator 자체의 정확도 검증 (RPS 계측, 503 카운팅, 히스토그램).

```bash
go test ./tools/loadgen/... -v
```

### 2. Chaos Test — Phase 4 전용 (Ring + gRPC + Health Probe)

Phase 4의 Consistent Hashing Ring과 gRPC 포워딩, 503 Fail-fast 정책을 실제 3노드 클러스터에서 검증합니다.

```bash
# Phase 4 바이너리 빌드
go build -o /tmp/core-x ./cmd/

# 환경변수 설정 후 실행
CORE_X_BINARY=/tmp/core-x go test ./tools/chaos/... -v -timeout 120s
```

> **전제**: `CORE_X_BINARY`가 없으면 테스트가 skip됩니다.

### 3. Phase별 성능 비교 벤치마크 (Phase 1~4)

각 Phase를 순서대로 기동하고 동일한 부하 파라미터로 측정한 뒤 비교 테이블을 출력합니다.

```bash
bash scripts/bench_phases.sh
```

기본 파라미터: `RPS=5000`, `concurrency=50`, `duration=15s`, `warmup=3s`

### 4. 단일 인스턴스 CLI 벤치마크

```bash
# 서버 실행
go run ./cmd/ &

# 특정 Phase 측정
go run ./tools/benchmark/ \
  --addr http://127.0.0.1:8080 \
  --rps 5000 \
  --concurrency 50 \
  --duration 15s \
  --warmup 3s \
  --phase "Phase 2: WAL + Hash Index KV"

# 최대 처리량 측정 (open-loop, rate-limiter 없음)
go run ./tools/benchmark/ \
  --addr http://127.0.0.1:8080 \
  --open-loop \
  --concurrency 100 \
  --duration 10s
```

---

## Chaos Test 시나리오 (Phase 4)

모든 시나리오는 Phase 4 컴포넌트(`ring.go`, `membership.go`, gRPC 포워더)를 검증합니다.

### TestChaos_NodeKillRecovery
**검증 대상**: Ring 포워딩 중 노드 다운 시 나머지 노드 생존 여부

3노드 부하 중 node-2를 `kill -9`로 강제 종료. Ring이 node-2 범위 키에 대해 503을 반환하는 동안 node-1, node-3은 정상 처리를 유지하는지 확인.

### TestChaos_CascadingFailurePrevention
**검증 대상**: 503 Fail-fast 정책 — 장애가 다른 노드로 전파되지 않음

node-2 장애 후 node-1, node-3의 RPS와 에러율이 정상 범위인지 측정. Cascading Failure 발생 여부 확인.

### TestChaos_HealthProbeLatency
**검증 대상**: `membership.go`의 Health Probe 감지 속도

`kill -9` 시점부터 첫 503 반환까지의 레이턴시 측정. SLO: **2초 이내**.

### TestChaos_SingleNodeMode
**검증 대상**: Phase 4 바이너리의 단일 노드 기본 성능 (gRPC 포워딩 오버헤드 없음)

---

## 실제 측정 결과 (2026-04-10)

환경: macOS Darwin 25.3.0 / Apple M-series / Go 1.25.5

### Load Generator 단위 테스트

| 테스트 | Phase 연관 | 결과 |
|--------|-----------|------|
| TestGenerator_BasicRun | 공통 | PASS — 499.5 RPS, p50=256µs, p99=512µs |
| TestGenerator_OpenLoop | 공통 | PASS — **88,937 RPS** (open-loop 최대 처리량) |
| TestGenerator_503Counting | Phase 4 Fail-fast | PASS — 503 rate 정확히 50% 계측 |
| TestHistogram_* | 공통 | PASS — **7.5ns/op, 0 alloc** |

### Chaos Test (Phase 4)

| 시나리오 | 검증 컴포넌트 | 결과 | 소요 시간 |
|----------|------------|------|-----------|
| NodeKillRecovery | Ring + gRPC 포워더 | **PASS** | 17.51s |
| CascadingFailurePrevention | 503 Fail-fast + membership | **PASS** | 19.24s |
| HealthProbeLatency | Health Probe (membership.go) | **PASS** | 2.23s |
| SingleNodeMode | Phase 4 기본 처리량 | **PASS** | 5.21s |

#### NodeKillRecovery 상세 (Ring + gRPC)

- 총 요청: 6,460건 / 부하 중 node-2 kill -9
- 에러: 503=0, 429=0, 네트워크 에러=0 (kill 시점 이전 요청 기준)
- Latency: mean=554µs, p50=256µs, p95=4ms, p99=8ms
- node-1, node-3 Cascading Failure 없음 ✅

#### CascadingFailurePrevention 상세 (503 Fail-fast)

- Baseline (3노드 정상): RPS=198.4, errRate=**0.00%**, p99=16ms
- node-2 kill 후 — node-2 키 범위 요청: errRate=100% (**의도된 Fail-fast**, 버그 아님)
- 살아남은 노드 RPS: 199.2 (임계값 99.2 초과 유지) ✅
- 네트워크 에러 생존 노드: 0건 ✅

#### HealthProbeLatency 상세 (membership.go)

- `kill -9` → 첫 503 감지까지: **11.32ms**
- 2s SLO 대비 **178배 빠름** ✅
- 감지 메커니즘: gRPC 연결 실패(즉시) → probe ticker 대기 없음

---

## 설계 결정 및 트레이드오프

### Fail-fast 정책 (Phase 4)
node-2 키 범위로 라우팅된 요청은 503을 반환합니다. 현재 정책은 재라우팅(rebalancing) 없이 Fail-fast만 수행합니다. 이를 변경하려면 `ring.go`의 failover 로직과 `membership.go`의 상태 전파를 수정해야 합니다.

### Health Probe 간격 (Phase 4)
`defaultProbeInterval = 5s`이지만 실제 감지는 gRPC 연결 실패(즉시)로 이루어져 11ms 수준. probe interval을 줄여도 감지 속도는 거의 변하지 않고 probe 트래픽만 증가합니다.

### 히스토그램 정밀도 (공통)
Power-of-2 bucket 사용 → p99 ±50% 오차 (e.g., 실제 7ms가 8ms 버킷에 기록됨). 엄격한 SLO 측정이 필요하면 `loadgen/histogram.go`를 linear bucket으로 교체하세요.

### Phase 1 분리 한계
현재 바이너리는 항상 WAL을 초기화합니다. `bench_phases.sh`에서 Phase 1을 완전히 분리하려면 빌드 태그(`//go:build no_wal`)나 env flag 우회가 필요합니다. 현재 스크립트는 WAL 경로만 다르게 설정합니다.
