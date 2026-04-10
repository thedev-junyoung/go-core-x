---
name: Load Generator and Chaos Test Tooling
description: tools/ 디렉토리에 load generator, chaos test, benchmark runner 구현 완료 — 사용법과 설계 결정 포함
type: project
---

Load Generator와 Chaos Test Suite가 `tools/` 디렉토리에 구현되어 있다.

**Why:** 10k RPS 검증 및 Phase별 성능 비교, 3노드 클러스터 fail-fast/recovery SLO 검증 필요.

**How to apply:** 신규 기능 추가 시 이 도구로 성능 회귀를 확인할 것.

## 디렉토리 구조

- `tools/loadgen/` — Load Generator 라이브러리
  - `histogram.go`: power-of-2 bucket, lock-free (atomic), 7.5ns/op 0 alloc
  - `generator.go`: fixed worker pool + rate-limiter ticker, open-loop/closed-loop 지원
  - `report.go`: 단상/다상 비교 리포트 출력
- `tools/benchmark/main.go` — CLI 벤치마크 러너 (서버 기동 후 실행)
- `tools/chaos/` — Chaos Test Suite
  - `cluster.go`: OS 프로세스 기반 3노드 클러스터 관리
  - `chaos_test.go`: NodeKillRecovery, CascadingFailurePrevention, HealthProbeLatency
- `scripts/bench_phases.sh` — 4-phase 자동 비교 스크립트

## 실측치 (Apple M1 Pro, httptest.Server 대상)

- Load generator max throughput: ~70k RPS (open-loop, httptest)
- Real Core-X server (Phase 2): ~9.5k RPS at 10k RPS target, p50=128µs, p99=8ms

## Chaos Test 실행 방법

```bash
go build -o /tmp/core-x ./cmd/
export CORE_X_BINARY=/tmp/core-x
go test ./tools/chaos/ -v -timeout 120s -run TestChaos
```

## 설계 결정

- OS 프로세스 격리: goroutine fake-kill은 TCP RST, 파일핸들 등 실제 장애 모사 불가
- 503 detection SLO = 2s, 현재 probeInterval=5s → 실제 detection ≈ 5–10s
  - SLO 충족하려면 probeInterval을 1s로 줄여야 함 (트레이드오프: gRPC probe 트래픽 5x 증가)
- 히스토그램 정밀도: ±50% (power-of-2 bucket) — p99 테일에서 허용 가능
