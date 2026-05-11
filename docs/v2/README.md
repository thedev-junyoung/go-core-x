# Core-X v2 — Correctness Over Consensus

> v1이 Raft를 **구현**한 시간이었다면, v2는 Raft를 **올바르게 사용**하는 시간이다.

**Status: Complete** (Phase 10–12, ADR-019–021)

---

## v2 완료 요약

| Phase | 주제 | ADR | PR | 상태 |
|---|---|---|---|---|
| 10 | Linearizable Read (ReadIndex + Lease Read) | ADR-019 | #73 | ✅ |
| 11 | Joint Consensus (동적 멤버십 변경) | ADR-020 | #75 | ✅ |
| 12 | Storage Unification (단일 Raft-backed KV) | ADR-021 | #76 | ✅ |

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
GET /kv/{key} (ReadIndex):
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
```

### 구현된 것

- `RaftNode.ReadIndex(ctx) (uint64, error)` — quorum heartbeat + commitIndex 반환
- `RaftNode.ReadIndexLease(ctx)` — clock-based lease, quorum 생략
- `UnifiedKVHandler` — ReadIndex → WaitForIndex → sm.Get 전체 파이프라인
- 독립 context budget: ReadIndex 2s + WaitForIndex 2s (합산 아님)

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

### 구현된 것

- `ClusterConfig` — `Voters []string`, `OldVoters []string` (joint 상태 표현)
- `ClusterConfig.IsJoint() bool`, `ClusterConfig.AllPeers() []string`
- `quorumSize()` — joint consensus 중 양쪽 quorum 독립 계산
- `POST /raft/config` — 노드 추가/제거 API
- `ClusterConfig.IsZero()` — snapshot 복구 시 빈 config 안전 처리
- `EnsureConnected` on snapshot restore — 복구 후 peer 연결 재수립

---

## Phase 12: Storage Unification (ADR-021)

### 기존 구조 (v1, ADR-018에서 의도적으로 분리)

```
POST /ingest → Consistent Hashing → ring.Lookup → Bitcask KV (WAL)
GET  /kv/{key} → KVStateMachine (in-memory, Raft-backed)
```

같은 시스템인데 쓰기와 읽기가 **다른 스토리지**를 바라봤다.
이 분리는 v1에서 학습 목적으로 의도적이었다.

### v2에서의 통합

```
POST /ingest → RaftNode.Propose → KVStateMachine.Apply → 204
GET  /kv/{key} → ReadIndex → WaitForIndex → KVStateMachine.Get → 200

ring은 "어느 노드가 담당하나" (파티셔닝 레이어)
Raft는 "그 파티션이 어떻게 합의하나" (복제 레이어)
```

### 구현된 것

- `RaftGroupRegistry` — PartitionID → *RaftNode thread-safe 매핑
- `NewHTTPHandler` 단일 생성자 — legacy 생성자 3개 제거
- `serveUnifiedIngest` — not-leader 시 307 redirect (addrMap 기반)
- `CORE_X_STORAGE_UNIFIED` feature flag 완전 제거
- 독립 context budget: Propose timeout 5s (ReadIndex/WaitForIndex와 별도)
- chaos audit 반영: write path redirect 대칭성, ctx 분리, duplicate Register 경고

---

## v1 → v2 학습 맵

| 단계 | Phase | 주제 | DDIA |
|------|-------|------|------|
| v1 | 1–2 | WAL, Bitcask KV, zero-allocation write path | Ch.3 |
| v1 | 3 | Consistent Hashing, gRPC forwarding | Ch.6 |
| v1 | 4 | Read path, observability | Ch.3 |
| v1 | 5a–5d | Raft leader election, log replication, write path | Ch.7, Ch.9 |
| v1 | 6 | Raft KV state machine + HTTP 연동 | Ch.9 |
| v1 | 7 | Multi-node 통합 테스트, leader redirect | Ch.9 |
| v1 | 8 | WAL + Bitcask storage integration (durability) | Ch.3, Ch.9 |
| v1 | 9 | Raft snapshot (log compaction) | Ch.9 |
| **v2** | **10** | **Linearizable Read (ReadIndex, Lease Read)** | **Ch.9** |
| **v2** | **11** | **Joint Consensus (동적 멤버십)** | **Ch.9** |
| **v2** | **12** | **Storage Unification** | **Ch.3 + Ch.9** |

---

## 시스템이 보장하는 것 (v2 기준)

| 보장 | 메커니즘 |
|---|---|
| Write durability | Raft log → WAL-backed apply |
| Write linearizability | Propose → committed → WaitForIndex → 204 |
| Read linearizability | ReadIndex quorum confirm → WaitForIndex → sm.Get |
| Split-brain prevention | Joint consensus C_old,new → C_new |
| Membership safety | 두 단계 전환 — 단일 quorum 항상 유지 |
| Leader redirect | 307 (addrMap 있을 때) / 503 (없을 때) |
| Snapshot safety | IsZero() guard + EnsureConnected on restore |
