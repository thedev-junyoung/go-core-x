# Phase 5b: 영속화 · 로그 복제 · 역할 전환 — 설계 근거

> **대상 커밋 범위**: `e98984dc` (Phase 5b Step 1) ~ 현재 미커밋 작업
> **참고 ADR**: [ADR-011](adr/011-raft-log-persistence-and-role-transition.md)

이 문서는 *왜* 이렇게 구현했는가에 집중한다.
*무엇을* 구현했는지는 ADR-011을, *어떻게 동작하는지*는 코드와 테스트를 읽어라.

---

## 1. MetaStore — 왜 따로 파일을 만들었나 (`e98984dc`)

### 문제

Phase 5a까지 `currentTerm`과 `votedFor`는 메모리에만 존재했다. 재시작하면 두 값이 0/"" 으로 초기화된다.

```
노드가 term=3에서 node-2에게 투표하고 crash.
재시작 후 term=0, votedFor="" 상태.
term=3 RequestVote가 다시 오면 → 다른 candidate에게 재투표 가능.
결과: 같은 term에서 두 개의 leader가 당선될 수 있다 (§5.2 위반).
```

Raft 논문 §5는 이 두 값을 **persistent state**로 명시한다 — 크래시 이후에도 반드시 보존해야 한다.

### WAL에 embed하지 않은 이유

가장 직관적인 방법처럼 보이지만 세 가지 문제가 있다.

**O(WAL) 복구 비용**: 재시작마다 수 GB의 WAL을 전체 스캔해서 가장 최근 term 레코드를 찾아야 한다. 이는 startup latency SLO에 직접 영향을 준다.

**Compaction 복잡도**: WAL compaction은 오래된 레코드를 제거한다. term 레코드는 절대 제거하면 안 된다는 불변식을 compaction 로직이 별도로 관리해야 한다. 이 불변식은 미묘하고, 위반해도 즉각적인 증상이 없어서 발견이 늦다.

**I/O 경합**: term 쓰기는 데이터 append와 같은 fd를 공유한다. 락 경합이 생기거나, atomic 보장을 위해 별도 sync point가 필요해진다.

### 269바이트 고정 크기 파일을 선택한 이유

```
[Version  : 1 byte ]
[Term     : 8 bytes] (int64, little-endian)
[VoteLen  : 1 byte ]
[VotedFor : 255 bytes] (zero-padded)
[CRC32    : 4 bytes]
─────────────────────
합계: 269 bytes
```

- **O(1) 복구**: 파일 전체가 레코드 1개. ReadAt(0)이면 끝이다.
- **Sector-atomic write**: 269 bytes < 512 bytes (disk sector). offset 0에서의 단일 `pwrite(2)`는 torn write가 발생하지 않는다.
- **CRC32**: torn write나 bit rot를 감지한다. 손상된 파일로 잘못된 term/vote를 복구하는 것보다 에러를 반환하는 것이 더 안전하다.
- **null로 시작 = 초기 상태**: 파일이 없거나 비어 있으면 `term=0, votedFor=""`. 첫 실행에 파일 초기화 로직이 불필요하다.

### Save 실패 = RPC 거부

```go
// vote grant 직전에 Save. 실패하면 vote를 주지 않는다.
if n.meta != nil {
    if err := n.meta.Save(RaftMeta{Term: n.currentTerm, VotedFor: candidateID}); err != nil {
        slog.Error("raft: failed to persist vote, denying to preserve safety", "err", err)
        return n.currentTerm, false  // vote 거부
    }
}
n.votedFor = candidateID  // persist 성공 후에만 메모리 업데이트
```

Save가 성공한 후에만 in-memory `votedFor`를 바꾼다. 순서가 반대면:
- `votedFor = candidateID` → 응답 전송 → crash → 재시작 후 `votedFor = ""` → 다시 투표 가능 → double voting.

`runCandidate`에서 self-vote persist 실패 시 term을 rollback하는 이유도 동일하다:

```go
if err := n.meta.Save(RaftMeta{Term: term, VotedFor: n.id}); err != nil {
    n.currentTerm--   // 인크리멘트 취소
    n.votedFor = ""
    n.role = RoleFollower
    n.mu.Unlock()
    return            // 선거 중단
}
```

term이 올라간 상태로 선거를 중단하면, 이 노드가 재시작 후 `term+1` 이전 리더의 heartbeat를 거부한다. rollback이 필수다.

---

## 2. `runCandidate` hardcoding 수정 — 왜 §5.4.1이 '코드는 있지만 동작 안 했나' (`c7ad28f`)

Phase 5a에서 `candidateLogUpToDate`는 구현됐지만 `runCandidate`가 peers에게 `RequestVote(term, n.id, **0, 0**)` 로 하드코딩해서 전송했다.

```
투표자 입장: candidate의 lastLogIndex=0, lastLogTerm=0
내 로그: lastLogIndex=5, lastLogTerm=3
candidateLogUpToDate(0, 0): candidateLastLogTerm(0) < myLastLogTerm(3) → false → DENY

하지만 투표자도 lastLogIndex=0이라면?
candidateLogUpToDate(0, 0): 0 >= 0 → true → GRANT
```

Phase 5a에서는 모든 노드의 `lastLogIndex=0`이었으므로 이 버그가 드러나지 않았다. Phase 5b에서 실제 로그 엔트리가 append되기 시작하면 §5.4.1이 완전히 무력화된다.

수정은 단순하다:
```go
// before
peer.RequestVote(term, n.id, 0, 0)

// after
peer.RequestVote(term, n.id, lastLogIndex, lastLogTerm)
// (runCandidate 시작 시 mu를 잡고 실제 값을 캡처)
```

---

## 3. §5.3 Log Replication — 왜 AppendEntries가 복잡해졌나

### 단순 heartbeat의 한계

Phase 5a의 `AppendEntries`는 `(term, leaderID)` 두 필드만 전송했다. 이것으로는 실제 로그 엔트리를 전달할 수 없고, follower가 자신의 로그가 leader와 일치하는지 확인할 방법도 없다.

### prevLogIndex/prevLogTerm — §5.3 Log Matching Property

Raft §5.3의 핵심 불변식:

> 두 노드의 로그에 같은 index, 같은 term의 엔트리가 있으면, 그 index까지의 **모든 엔트리**가 동일하다.

이 불변식을 강제하는 메커니즘이 `prevLogIndex/prevLogTerm` 일관성 체크다:

```
Leader → Follower: AppendEntries{
    prevLogIndex: 4,   // 새 엔트리 직전 인덱스
    prevLogTerm:  2,   // 그 인덱스의 term
    entries:      [{index:5, term:3, data:...}],
    leaderCommit: 4
}

Follower:
  내 log[4]의 term == 2? 아니면 → conflictIndex/conflictTerm 반환 → reject
  맞으면 → 5번 엔트리 append → success
```

reject 이후 leader가 `nextIndex`를 줄여가며 재시도하면 결국 두 로그가 공통 prefix를 찾아 sync된다.

### Fast Backup — O(n) → O(1) nextIndex 조정

나이브한 구현은 reject당할 때마다 `nextIndex--` 한 번씩 줄인다. 로그가 1000개 뒤처진 follower는 1000번의 RPC가 필요하다.

`conflictTerm`을 이용한 Fast Backup:

```go
// Follower response on mismatch:
conflictTerm  = n.log[prevLogIndex].term  // 충돌 term
conflictIndex = 첫 번째로 conflictTerm이 등장하는 인덱스

// Leader 처리:
if leaderHas(conflictTerm):
    nextIndex = leader의 conflictTerm 마지막 인덱스 + 1
else:
    nextIndex = conflictIndex  // leader에게 없는 term → 통째로 건너뜀
```

같은 term의 엔트리들은 leader에도 있거나 통째로 없다(Log Matching Property). 따라서 term 단위로 건너뛰면 O(distinct terms)번의 RPC로 sync된다.

### §5.4.2 Current Term Only Commit

Leader는 **현재 term의 엔트리**만 quorum commit한다:

```go
func (n *RaftNode) maybeAdvanceCommitIndex(currentTerm int64) {
    for idx := n.lastLogIndex; idx > n.commitIndex; idx-- {
        // ★ 이전 term의 엔트리는 quorum을 달성해도 직접 commit하지 않는다
        if n.entryAt(idx).Term != currentTerm {
            continue
        }
        // ...quorum check...
    }
}
```

왜? 이전 term의 엔트리를 직접 commit하면 "Figure 8" 시나리오에서 committed entry가 덮어써질 수 있다. 현재 term 엔트리가 commit되면 Log Matching Property에 의해 이전 모든 엔트리도 간접적으로 commit된다.

---

## 4. RoleController — 왜 callback이 아닌 polling인가

### Callback의 문제

가장 반응성 좋은 방법은 `runLeader()`가 리더가 되는 순간 `onBecomeLeader()`를 직접 호출하는 것이다. 하지만 `runCandidate`/`runLeader`는 `n.mu`를 잡고 실행된다:

```
runCandidate():
    n.mu.Lock()
    n.role = RoleLeader
    onBecomeLeader()  ← mu 보유 상태
        → ReplicationManager.BecomeLeader()
            → gRPC stream 시작
                → streamer가 WAL 읽기 시도
                    → WAL이 RaftNode 상태 조회 (n.mu.Lock() 재시도)
                        → DEADLOCK
```

또는 `RaftNode`가 `ReplicationManager`를 직접 참조하면 계층이 역전된다:

```
합의 계층 (raft/)  →  전송 계층 (replication/)  ← 의존성 방향 위반
```

`RaftNode` 단위 테스트에 replication mock이 끼어든다.

### Polling이 충분한 이유

```
HeartbeatInterval = 50ms
RoleController poll = 50ms
```

새 리더가 당선되면 첫 heartbeat를 보내기 전에 이미 RoleController가 역할 변화를 감지한다. 실질적인 쓰기 가용성 지연은 없다.

```
role: Follower → Leader (선출 완료)
        ↓ 최대 50ms 대기
RoleController poll
        ↓
ReplicationManager.BecomeLeader()
        ↓
StreamWAL 활성화
        ↓ 50ms 이내
첫 heartbeat 전송
```

---

## 5. ManagedReplicationServer — 왜 null-object 패턴인가

`grpc.Server`는 `Serve()` 시작 이후 handler 재등록을 지원하지 않는다. 역할이 바뀔 때마다 gRPC 서버를 재시작할 수는 없다 (포트 binding 비용, in-flight RPC 강제 종료).

해결책: handler는 항상 등록해두되, **요청 처리 여부**를 atomic flag로 제어한다.

```go
func (s *ManagedReplicationServer) StreamWAL(req *pb.StreamWALRequest, stream ...) error {
    if s.active.Load() == 0 {
        return status.Error(codes.Unavailable, "this node is not the current leader")
    }
    // 실제 처리
}
```

replica는 `codes.Unavailable`을 받으면 현재 리더를 다시 찾아 재연결한다. 서버 재시작 없이 역할 전환이 클라이언트에게 투명하게 전달된다.

---

## 설계 결정 요약

| 결정 | 선택 | 기각된 대안 | 핵심 이유 |
|---|---|---|---|
| 영속화 위치 | `raft_meta.bin` (별도 파일) | WAL embed | O(1) 복구, compaction 단순화 |
| 파일 포맷 | 269B 고정 크기 + CRC32 | JSON / protobuf | sector-atomic write |
| Save 실패 처리 | RPC 거부 | 경고 후 계속 | Safety > Availability |
| AppendEntries 최적화 | Fast Backup (conflictTerm) | nextIndex-- 1씩 | O(distinct terms) sync |
| quorum commit | 현재 term만 직접 commit | 모든 term | §5.4.2, Figure 8 방지 |
| 역할 전환 통지 | 50ms polling | callback | 데드락 방지, 계층 결합 해소 |
| gRPC 비활성화 | atomic flag (null-object) | 서버 재시작 | 서버 재등록 불가 제약 |
