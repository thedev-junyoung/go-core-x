# Phase 13 — MVCC Overhead Benchmark

Phase 13 (ADR-022) MVCC + CAS 도입 후, 기존 KVStateMachine 대비 오버헤드를 측정한 결과.

---

## 측정 환경

- **CPU**: Apple M-series (15 logical cores 가용)
- **Go**: project standard toolchain
- **벤치마크 도구**: `go test -bench` (in-process, no Raft round-trip)
- **시행 횟수**: `-benchtime=3s -count=3` (3회 반복, 평균)
- **벤치마크 파일**: `internal/infrastructure/raft/mvcc_overhead_bench_test.go`

## 측정 범위

각 state machine의 `apply()` 호출만 측정. Raft consensus(quorum fsync, network), HTTP 핸들러 비용은 포함되지 않음.
순수 MVCC 자료구조 오버헤드를 격리하기 위함.

| 벤치마크 | 측정 대상 |
|---|---|
| `BenchmarkKV_UnconditionalWrite` | `KVStateMachine.apply` — baseline (단일 버전 맵 쓰기) |
| `BenchmarkKV_Get` | `KVStateMachine.Get` — baseline (단일 맵 lookup) |
| `BenchmarkMVCC_UnconditionalWrite` | `MVCCStateMachine.apply`, `ExpectedVersion=0` (버전 히스토리 추가) |
| `BenchmarkMVCC_Get` | `MVCCStateMachine.Get` — 최신 버전 lookup |
| `BenchmarkMVCC_CASSuccess` | CAS 성공 경로 (버전 일치 → 새 버전 append) |
| `BenchmarkMVCC_CASConflict` | CAS 충돌 경로 (버전 불일치 → conflict 기록, 조기 종료) |

---

## 결과 (3회 평균)

| Benchmark | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| `KV_UnconditionalWrite` | **377** | 336 | 10 |
| `KV_Get` | **6.17** | 0 | 0 |
| `MVCC_UnconditionalWrite` | **416** | 408 | 12 |
| `MVCC_Get` | **10.49** | 0 | 0 |
| `MVCC_CASSuccess` | **687** | 672 | 14 |
| `MVCC_CASConflict` | **663** | 380 | 11 |

---

## 분석

### 1. 쓰기 경로 — MVCC overhead는 ~10%

```
KV write:                 377 ns/op
MVCC unconditional write: 416 ns/op  (+39 ns, +10.3%)
```

MVCC가 단순 쓰기에 추가하는 비용:
- `versions[key]`에 `MVCCVersion` slice append (+1 alloc)
- `latest[key]` map write (덮어쓰기, 신규 키만 alloc)
- 총 **+2 allocs, +72 B**

**해석**: 버전 히스토리를 유지하기 위한 추가 비용이 10% 수준. event sourcing이나 audit trail이 필요한 워크로드에 충분히 감당 가능한 범위.

### 2. 읽기 경로 — 절대치는 ns 단위지만 상대 차이는 ~70%

```
KV Get:   6.17 ns/op
MVCC Get: 10.49 ns/op  (+4.32 ns, +70%)
```

MVCC `Get`은 `latest[key]` → `versions[key][last]` 두 단계 lookup. 단순 맵 조회 1회 vs 2회 인디렉션.

**해석**: 절대치는 무시할 수준이지만, 핫 패스에 있는 read-heavy 워크로드라면 차이가 누적될 수 있음. 단, 실제로는 ReadIndex protocol(ADR-019)의 네트워크 RTT가 훨씬 dominant하므로 무시 가능.

### 3. CAS 경로 — 성공/실패 모두 ~65% 추가 비용

```
MVCC unconditional: 416 ns/op
MVCC CAS success:   687 ns/op  (+271 ns)*
MVCC CAS conflict:  663 ns/op  (+247 ns)
```

\* `CASSuccess`는 `ExpectedVersion`이 매 iteration마다 달라야 해서 JSON marshal이 hot loop에 포함됨. marshal 오버헤드(~40–60ns) 차감하면 실제 apply-only 비용은 ~620–640ns.

CAS 성공/실패의 차이가 작은 이유:
- 두 경로 모두 JSON unmarshal + mutex lock + map lookup이 dominant
- 성공: append + latest update
- 실패: conflictResults map write + early return
- 본질적인 work 양이 비슷함

**해석**: 동시 read-modify-write에서 lost update를 막기 위한 정직한 cost. PostgreSQL의 optimistic locking, etcd의 `WithCompareValue` 등도 비슷한 오버헤드를 감수한다.

### 4. 메모리 — MVCC가 21% 더 큼 (쓰기 기준)

```
KV write:                 336 B
MVCC unconditional write: 408 B  (+72 B, +21%)
MVCC CAS success:         672 B  (+264 B, +79%)
```

MVCC는 키마다 버전 slice를 들고 있어서 메모리 사용량이 더 큼. `retention` 파라미터로 키당 최대 버전 수를 제한 가능 (현재 기본 10).

CAS success가 추가로 큰 이유: 매 iteration마다 새 JSON marshal이 allocation 발생.

---

## 시사점

### 실무 매핑

| 워크로드 | 추천 |
|---|---|
| Append-only 이벤트 수집 (Kafka-like) | KVStateMachine 충분 |
| 일반 KV (set/get, 충돌 드뭄) | MVCC unconditional 무방 (10% 오버헤드) |
| Counter, 잔액 등 동시 갱신 | CAS 필수 (65% 오버헤드 감수) |
| Audit trail / 시점 조회 필요 | MVCC + `?version=N` snapshot read |

### 면접 답변 예시

> "MVCC를 도입했을 때 성능 영향을 측정한 적이 있나요?"
>
> 네, in-process 벤치마크 기준으로 단순 쓰기는 ~10% 오버헤드(377ns → 416ns),
> CAS 경로는 ~65% 오버헤드(~690ns)였습니다. 메모리는 키당 버전 slice 때문에
> ~21% 증가했고, retention 파라미터로 제어 가능합니다.
> 다만 이 수치는 Raft consensus나 네트워크 RTT를 제외한 자료구조 자체의
> 비용이라, 실제 end-to-end p99 latency(수 ms 단위)에서는 비율이 훨씬 작아집니다.

### 측정 한계

1. **In-process 벤치마크**: Raft round-trip, fsync, gRPC 비용 미반영. 실제 latency p99는 ms 단위.
2. **단일 키 hot path**: 키별 mutex contention 없음. 실제 운영에서는 키 분산에 따라 다름.
3. **CASSuccess의 marshal-in-loop**: `ExpectedVersion` 단조 증가 때문에 hot loop에서 marshal 발생. 실제 워크로드(매 요청마다 다른 expected version)와 일치하므로 의도된 측정.
4. **conflictResults map 무한 증가**: 벤치마크에서 GC가 되지 않아 map 크기가 누적. 실제로는 `IsCASConflict` 호출 후 삭제됨.

---

## 재현 방법

```bash
go test -run=^$ \
  -bench='^(BenchmarkKV_|BenchmarkMVCC_)' \
  -benchmem -benchtime=3s -count=3 \
  ./internal/infrastructure/raft/
```

벤치마크 추가/수정 위치: `internal/infrastructure/raft/mvcc_overhead_bench_test.go`
