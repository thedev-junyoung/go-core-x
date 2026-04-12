# ADR-013: Raft Write Path — Propose + Apply Channel

**상태**: Accepted
**날짜**: 2026-04-12
**관련**: [ADR-012](012-raft-log-wal-persistence.md)

---

## 문제

Phase 5c까지 Raft 로그는 follower가 `HandleAppendEntries`로 엔트리를 *받는* 방향만 구현됐다.
Leader가 클라이언트 요청을 **직접 로그에 쓰는** 경로, 그리고 커밋된 엔트리를 상태 머신에
전달하는 경로가 없었다.

구체적으로 두 가지가 빠져 있었다.

1. **Propose**: leader가 새 엔트리를 자신의 로그에 append하고, 다음 heartbeat에서 followers에게 복제를 시작하는 진입점.
2. **Apply channel**: `commitIndex`가 `lastApplied`를 추월했을 때 커밋된 엔트리를 상태 머신 소비자에게 전달하는 경로.

`lastApplied`는 Phase 5b부터 필드로 선언되어 있었지만 사용되지 않았다.

---

## 결정

### Propose API

```go
func (n *RaftNode) Propose(data []byte) (index int64, term int64, isLeader bool)
```

- leader가 아닌 경우 즉시 `isLeader=false` 반환 — 호출자가 redirect 처리.
- leader인 경우: `logStore.Append()` → 영속화 성공 후 `n.log` append → 반환.
- 커밋은 **비동기**: 다음 heartbeat에서 followers에게 복제되고 quorum 달성 시 `commitIndex`가 진전된다. `ApplyCh()`로 커밋 완료를 확인한다.

### Apply Channel

```go
func (n *RaftNode) ApplyCh() <-chan LogEntry
```

`Run()` 내에서 `runApplyLoop` 고루틴을 시작하며, 5ms 폴링으로 `commitIndex > lastApplied`를 감지한다. 해당 구간의 엔트리를 `applyCh (cap=256)`로 전달하고 `lastApplied`를 갱신한다.

### 단일 노드 commitIndex 진전

peers가 없는 single-node 배포에서 `sendHeartbeats`는 `len(peers)==0` 이면 즉시 반환하며 `maybeAdvanceCommitIndex`를 호출하지 않았다. 결과적으로 `Propose` 후 `commitIndex`가 영원히 진전되지 않았다.

수정: `runLeader`에서 `sendHeartbeats` 이후, peers가 없으면 `maybeAdvanceCommitIndex(term, lastIdx, majority=1)`을 직접 호출한다.

---

## 기각된 대안들

### Propose 시 즉시 commit (단일 노드 전용)

단일 노드에서 `Propose` 내에서 `commitIndex`를 즉시 올리는 방법이다. 클러스터 모드와 코드 경로가 달라지고 테스트가 복잡해진다. heartbeat tick에서 commit하는 단일 경로를 유지하는 것이 일관성이 높다.

### Callback 기반 Apply

`RaftNode`가 `onApply func(LogEntry)` 콜백을 받아 직접 호출하는 방법이다.

- `runApplyLoop`은 `n.mu`를 잠깐 잡고 해제한 뒤 channel에 전달한다. 콜백이면 콜백 내부에서 `n.mu`를 재획득하거나 RaftNode 상태를 조회할 경우 데드락이 발생한다.
- Channel 방식은 producer(apply loop)와 consumer(state machine)를 분리해 락 경합과 계층 결합을 모두 피한다.

### Condition Variable (sync.Cond)

`commitIndex` 진전 시 `sync.Cond.Broadcast()`로 apply loop를 깨우는 방법이다. 폴링(5ms) 대비 반응성이 높다.

기각 이유: 구현 복잡도 대비 이득이 작다. HeartbeatInterval이 50ms이므로 commit 이벤트도 최대 50ms 간격으로 발생한다. 5ms 폴링은 이 보다 훨씬 빠르므로 apply 지연은 5ms 이하로 충분하다.

---

## 설계 세부

### persist-before-in-memory (Propose)

```go
// 영속화 먼저
if n.logStore != nil {
    if err := n.logStore.Append(entry); err != nil {
        return 0, 0, false  // persist 실패 = non-leader로 취급
    }
}
// 영속화 성공 후 메모리 갱신
n.log = append(n.log, entry)
```

순서가 반대면 crash 시 메모리에는 있지만 WAL에는 없는 엔트리가 생겨 재시작 후 로그 불일치가 발생한다. `HandleAppendEntries`와 동일한 invariant.

### applyCh 버퍼 크기 (256)

256 = HeartbeatInterval(50ms) / applyPollInterval(5ms) × 예상 burst 엔트리 수.

정상 운영에서는 heartbeat당 소수의 엔트리만 커밋된다. 256은 소비자가 일시적으로 느려져도 apply loop를 블로킹하지 않는 충분한 여유다.

소비자가 장기적으로 느리면 channel이 가득 차고 apply loop가 블로킹된다. 이는 의도된 backpressure다 — apply loop를 무한히 앞서게 두면 `lastApplied`가 진전되어 이미 전달됐다고 표시된 엔트리가 재전달되지 않는다.

### runApplyLoop 스냅샷 패턴

```go
n.mu.Lock()
from := n.lastApplied + 1
to   := n.commitIndex
toApply := collect(from, to)   // mu 보유 중 스냅샷
n.lastApplied = to
n.mu.Unlock()

for _, e := range toApply {
    applyCh <- e               // mu 해제 후 전달
}
```

`applyCh <- e`를 mu 보유 중에 호출하면, 소비자 goroutine이 같은 mu를 획득하려 할 때 데드락이 발생한다. mu를 해제한 뒤 전달하는 이유다.

---

## 결과

| 속성 | 이전 | 이후 |
|---|---|---|
| Leader write | 불가 | `Propose()` → log append + persist |
| Apply 전달 | 없음 | `applyCh`로 커밋된 엔트리 순서 보장 전달 |
| Single-node commit | 미작동 | heartbeat tick마다 즉시 commit |
| `lastApplied` | 미사용 | apply loop가 추적 및 갱신 |
