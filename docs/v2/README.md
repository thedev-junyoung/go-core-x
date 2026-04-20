# Core-X v2 — Correctness Over Consensus

> v1이 Raft를 **구현**한 시간이었다면, v2는 Raft를 **올바르게 사용**하는 시간입니다.

---

## v1이 남긴 gap

v1은 Raft의 핵심 알고리즘을 전부 구현했다. Leader election, log replication, log compaction,
snapshotting, WAL-backed durability, E2E failover 테스트까지. 쓰기의 safety는 완전히 보장된다.

**그런데 읽기는?**

```
현재 GET /raft/kv/{key}:
  → leader 검증 없이 in-memory map 직접 읽기
  → 네트워크 파티션으로 leader가 격리되면 stale data 반환 가능
```

Raft가 guarantee하는 것은 "committed된 쓰기는 유실되지 않는다"뿐이다.
읽기의 linearizability는 별도로 보장해야 한다.

이것이 **v2의 출발점**이다.

---

## 왜 이게 실무에서 중요한가

**재고 시스템**: 마지막 재고 1개 차감 → 즉시 재고 조회 → stale read로 1개 있음 → 이중 판매

**결제 시스템**: 결제 완료 → 잔액 조회 → 이전 잔액 반환 → 사용자 혼란

**권한 시스템**: 관리자가 권한 박탈 → 박탈된 사용자가 여전히 API 접근

이 모든 문제는 **master-slave (leader-follower) 구조에서 읽기 경로를 올바르게 설계하지 않으면 발생**한다.
MySQL replication lag, Redis Sentinel failover, etcd의 기본 read — 동일한 문제 클래스다.

---

## v2 목표

| # | 주제 | 핵심 문제 | DDIA |
|---|------|----------|------|
| 1 | **Linearizable Read** | GET이 stale read를 반환할 수 있음 | Ch.9 Linearizability |
| 2 | **Joint Consensus** | 클러스터 멤버십 변경 중 safety 보장 | Ch.9 §6 |
| 3 | **Storage Unification** | 두 개의 write path → 하나의 Raft-backed storage | Ch.3 + Ch.9 |

---

## Phase 10: Linearizable Read (ADR-019)

### 문제

```
시나리오: 3-node cluster, 네트워크 파티션

  [node-1 (old leader)] --- 격리 ---  [node-2, node-3]
         |
         ↓ 새 leader 선출 (node-2)

  node-1은 자기가 아직 leader라고 착각
  node-1에 GET 요청 → stale data 반환 ← Linearizability 위반
```

### 해결: ReadIndex Protocol

```
GET /raft/kv/{key} (ReadIndex):
  1. leader가 현재 commitIndex를 기록 (readIndex)
  2. quorum에 heartbeat 전송 → "내가 아직 leader가 맞나?" 확인
  3. quorum 응답 수신 → leader 지위 확인됨
  4. appliedIndex >= readIndex 대기 (WaitForIndex)
  5. in-memory map 읽기 → 응답

보장: 읽기 시점의 commitIndex까지 apply된 상태를 반영
```

### 최적화: Lease Read

```
ReadIndex의 단점: 읽기마다 quorum heartbeat → ~1ms latency

Lease Read:
  leader가 마지막 heartbeat 이후 election timeout이 지나지 않았으면
  → quorum 확인 없이 바로 읽기 허용

이유: election timeout 내에 다른 leader가 선출될 수 없음
결과: 읽기 latency ~50μs (20x 개선)

단, monotonic clock이 필요 (system clock skew 주의)
```

### 구현 범위

- `RaftNode.ReadIndex(ctx) (uint64, error)` — quorum heartbeat + commitIndex 반환
- `GET /raft/kv/{key}` 핸들러 수정 — ReadIndex 호출 후 WaitForIndex 대기
- Lease Read 옵션 (`CORE_X_RAFT_LEASE_READ=true`)
- 통합 테스트: 파티션 시나리오에서 stale read 발생 → ReadIndex 적용 후 linearizable 확인

---

## Phase 11: Joint Consensus (ADR-020)

### 문제

```
3-node → 5-node 확장 시나리오:

  기존: [A, B, C]
  추가: [A, B, C, D, E]

  전환 순간 두 개의 quorum이 존재할 수 있음:
    old quorum: A+B (2/3)
    new quorum: A+D+E (3/5)

  → 두 quorum이 서로 다른 leader를 선출 가능 → split-brain
```

### 해결: C_old,new Joint Configuration

```
전환을 두 단계로 나눔:

  1단계: C_old,new 진입
    - 모든 결정에 old AND new 양쪽 quorum 동의 필요
    - 이 단계에서는 split-brain 불가능

  2단계: C_new 확정
    - new quorum만으로 결정
    - 전환 완료
```

### 구현 범위

- `ClusterConfig` — old/new 멤버십 표현
- `AppendEntries`에 config change log entry 타입 추가
- `quorumSize()` — joint consensus 중 양쪽 quorum 계산
- `POST /raft/config` — 노드 추가/제거 API
- 통합 테스트: 3→5 확장, 5→3 축소, 전환 중 leader election

---

## Phase 12: Storage Unification (ADR-021)

### 현재 구조 (ADR-018에서 의도적으로 분리)

```
POST /ingest → Consistent Hashing → Bitcask KV (WAL-backed)
POST /raft/kv → Raft consensus → KVStateMachine (in-memory + snapshot)
```

v1에서 이 분리는 **의도적**이었다: 각 path가 다른 DDIA 챕터를 학습하기 위해.

### v2에서의 통합

```
Consistent Hashing → Raft leader로 라우팅 (routing layer로 격상)
모든 쓰기 → Raft consensus → WAL-backed KVStateMachine
```

```
POST /ingest → ring.Lookup(key) → 담당 Raft group leader → Propose → Apply
GET /kv/{key} → ring.Lookup(key) → 담당 Raft group leader → ReadIndex → Apply
```

### 의미

Consistent Hashing이 "어떤 노드가 담당하나" (파티셔닝)를 결정하고,
Raft가 "그 파티션 내에서 어떻게 합의하나" (복제)를 담당한다.

이것이 **실제 분산 데이터베이스 (TiKV, CockroachDB)의 구조**다.

---

## v1 → v2 학습 맵

| 단계 | 주제 | DDIA |
|------|------|------|
| v1 Phase 1–3 | WAL, Bitcask, Consistent Hashing | Ch.3, Ch.6 |
| v1 Phase 5–9 | Raft 구현 (election, replication, snapshot) | Ch.7, Ch.9 |
| **v2 Phase 10** | **Linearizable Read (ReadIndex, Lease Read)** | **Ch.9** |
| **v2 Phase 11** | **Joint Consensus (dynamic membership)** | **Ch.9** |
| **v2 Phase 12** | **Storage Unification (Raft-backed Bitcask)** | **Ch.3 + Ch.9** |

---

## 시작점

**Phase 10 (Linearizable Read)** 가 v2의 첫 번째 목표다.

v1의 모든 읽기 경로가 correctness gap을 가지고 있고,
이것이 가장 빠르게 구현 가능하며 가장 즉각적인 correctness 개선을 제공한다.

ADR-019 작성 → 구현 → E2E 파티션 테스트 순서로 진행.
