# End-to-End Latency Benchmark — Single-node Raft

Phase 12b unified write path (`POST /ingest` → Raft Propose → KVStateMachine apply → 204)의
실측 end-to-end latency. Phase 13 in-process apply 비용(`docs/PHASE13_MVCC_BENCHMARK.md`)과
비교해, 자료구조 비용이 실제 production-like 환경에서 얼마나 묻히는지 정량적으로 확인.

---

## 측정 환경

- **노드 구성**: 단일 노드 Raft (1 node = 1 Raft group, peers 없음 → self가 즉시 leader)
- **WAL 설정**: `SyncPolicy=SyncInterval, SyncInterval=100ms` (cmd/main.go 기본값)
- **부하 발생기**: `tools/benchmark/` (`tools/loadgen/` 기반)
- **클라이언트**: localhost loopback (네트워크 RTT ≈ 0)
- **머신**: Apple M-series, 15 logical cores

## 측정 방법

```bash
# 빌드 및 서버 기동
go build -o /tmp/core-x-bench ./cmd/
CORE_X_NODE_ID=n1 CORE_X_GRPC_ADDR=127.0.0.1:9101 \
CORE_X_ADDR=:8101 CORE_X_RAFT_HTTP_NODES=n1=http://127.0.0.1:8101 \
CORE_X_WAL_PATH=/tmp/core-x-bench-data/events.wal \
CORE_X_SNAPSHOT_DIR=/tmp/core-x-bench-data/snapshots \
/tmp/core-x-bench > /dev/null 2>&1 &

# 부하 측정
go run ./tools/benchmark/ --addr http://127.0.0.1:8101 \
  --rps <target> --concurrency <n> --duration 10s --warmup 2s
```

`tools/loadgen/`의 히스토그램은 power-of-two 마이크로초 버킷(`docs/loadgen/histogram.go`)이라
tail latency는 logarithmic precision으로 보고됨 — p95와 p99가 같은 버킷에 떨어지는 경우가 빈번.

---

## 결과

| 시나리오 | Target RPS | Concurrency | Actual RPS | Mean | p50 | p95 / p99 |
|---|---:|---:|---:|---:|---:|---:|
| Low load | 100 | 5 | 100 | 29.5 ms | 32.8 ms | 65.5 ms |
| Medium load | 200 | 10 | 199 | 27.0 ms | 32.8 ms | 65.5 ms |
| Heavy load | 500 | 20 | ~470 | 68.5 ms | 131.1 ms | 131.1 ms |
| Saturation | open-loop | 50 | **306** | 163.4 ms | 262.1 ms | 262.1 ms |

**처리량 ceiling: ~306 RPS** (단일 노드 Raft, SyncInterval=100ms 기준)

---

## 분석

### 1. In-process apply vs end-to-end의 격차

| 측정 | Latency | 배율 |
|---|---:|---:|
| `MVCCStateMachine.apply` (in-process) | 416 ns ≈ 0.0004 ms | 1× |
| 단일 노드 Raft end-to-end p50 (low load) | 32.8 ms | **~78,000×** |
| 단일 노드 Raft end-to-end p99 (saturation) | 262 ms | **~630,000×** |

자료구조 비용은 실측 latency의 1/100,000 이하. 즉 Phase 13에서 측정한 MVCC 오버헤드 +10%(40ns)는
end-to-end에서는 측정 불가능한 수준의 차이가 됨. **벤치 결과의 ns 단위 차이는 실제 production
latency에서 사실상 사라진다**는 결론.

### 2. 단일 노드인데 왜 30~65ms p99인가

네트워크 RTT 0, replication 0인데도 32~65ms latency가 나오는 이유:

**주범: WAL fsync 100ms 배치 (`SyncInterval`)**
- Raft Propose → 로그 entry append → WAL 작성
- WAL은 모든 write를 100ms 윈도우로 모아서 fsync (개별 fsync ~5ms × 1000 ops/s vs batch fsync 100ms 분산 → throughput 우위)
- 한 요청은 들어온 시점부터 다음 sync까지 평균 50ms 대기

이것이 p99=65ms (≈ SyncInterval × 0.65) 의 정체. SyncInterval을 10ms로 낮추면 p99 ~10ms 가능
하지만 throughput은 떨어진다 (fsync 비용/op 증가).

### 3. RPS 증가에 따른 latency 가속화

- 100 RPS → p99 65ms
- 500 RPS → p99 131ms (**2배**)
- saturation → p99 262ms (**4배**)

closed-loop이 아닌데도 latency가 RPS에 비례해 증가하는 이유: 단일 노드 Raft는 모든 쓰기를
**leader가 직렬화**한다. RPS가 늘면 leader의 처리 큐가 길어져 평균 대기시간이 증가.

다중 파티션 Raft (Phase 12 scope 밖)이면 키 분산만큼 leader가 늘어 이 ceiling이 깨진다.
TiKV/CockroachDB가 Region per Raft group을 쓰는 이유가 바로 이 단일 leader bottleneck 회피.

### 4. 처리량 ceiling 분석 — 왜 306 RPS인가

```
306 RPS × 50 concurrent worker → 평균 latency = 50/306 ≈ 163ms (=> mean 일치)
```

서버는 들어오는 요청을 직렬화하면서 fsync batch를 기다린다. SyncInterval=100ms 기준 이론적 상한:
- 100ms 윈도우에 batch한 entry 수 × 10 batches/s ≈ 처리량
- 실측 306 RPS = 100ms 당 30개 ≈ 30 entries/batch

이건 단일 노드 SQLite fsync 한계와 동일한 패턴 (PostgreSQL `synchronous_commit=on` 기본값도
비슷한 ~500-1000 tps).

---

## 시사점

### 면접용 한 문장

> "Phase 13 MVCC apply 자체는 416ns지만, 단일 노드 Raft end-to-end p99는 65ms였습니다.
> 약 16만 배. 자료구조 비용은 의미가 없고, WAL fsync 정책(`SyncInterval=100ms`)이 latency의
> 거의 전부였습니다. Throughput ceiling은 ~306 RPS — 단일 leader fsync 한계."

### 개선 방향 (구현하지는 않음, 학습 목적 정리)

| 변경 | 예상 효과 | trade-off |
|---|---|---|
| `SyncInterval` 10ms로 단축 | p99 ~10ms로 감소 | fsync 횟수 10배 ↑, throughput ↓ |
| `SyncImmediate` | p99 ~5ms (디스크 fsync 비용만) | throughput 추가 하락 |
| `SyncNever` (위험) | p99 sub-ms 가능 | OS crash 시 데이터 손실 |
| 다중 Raft group (파티션당) | leader 직렬화 완화 | 구현 복잡도 ↑ (Phase 14 scope) |
| Raft 로그 batching 강화 | 단일 fsync에 더 많은 entry | 평균 latency ↑, throughput ↑ |

### 측정 한계

1. **단일 노드만 측정**: 실제 production은 3+ 노드 → follower fsync + 네트워크 RTT 추가. 측정은
   ~10ms 추가 예상 (LAN 기준). 진행하려면 `tools/chaos/Cluster`로 3-node 기동 후 동일 부하.
2. **localhost 측정**: 네트워크 RTT 0으로 클러스터 latency가 과소평가됨.
3. **히스토그램 정밀도**: power-of-two 버킷 → tail latency를 logarithmic 단위로만 관측 가능.
   p95/p99 구분이 어렵다.
4. **단일 키 부하**: 모든 요청이 같은 source key → ring routing 비용은 측정되나 multi-shard
   benefit은 측정 불가.
5. **로그 출력**: 서버 stdout을 `/dev/null`로 redirect해 logging overhead 제거. 운영 환경의
   structured log는 추가 latency 가능.

---

## 재현 방법

```bash
# 1. 빌드
go build -o /tmp/core-x-bench ./cmd/

# 2. 단일 노드 기동 (백그라운드)
rm -rf /tmp/core-x-bench-data && mkdir -p /tmp/core-x-bench-data
CORE_X_NODE_ID=n1 CORE_X_GRPC_ADDR=127.0.0.1:9101 \
CORE_X_ADDR=:8101 CORE_X_RAFT_HTTP_NODES=n1=http://127.0.0.1:8101 \
CORE_X_WAL_PATH=/tmp/core-x-bench-data/events.wal \
CORE_X_SNAPSHOT_DIR=/tmp/core-x-bench-data/snapshots \
/tmp/core-x-bench > /dev/null 2>&1 &

# 3. healthz 대기 후 부하
sleep 3 && curl -sf http://127.0.0.1:8101/healthz

# 4. 여러 부하 시나리오
go run ./tools/benchmark/ --addr http://127.0.0.1:8101 \
  --rps 100 --concurrency 5 --duration 10s --warmup 2s --phase "Low"
go run ./tools/benchmark/ --addr http://127.0.0.1:8101 \
  --rps 500 --concurrency 20 --duration 10s --warmup 2s --phase "Heavy"
go run ./tools/benchmark/ --addr http://127.0.0.1:8101 \
  --open-loop --concurrency 50 --duration 10s --warmup 2s --phase "Saturation"

# 5. 정리
kill %1
```
