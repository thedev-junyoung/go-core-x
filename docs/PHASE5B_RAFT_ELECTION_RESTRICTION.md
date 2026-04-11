# Raft §5.4.1 Election Restriction

## 왜 필요한가

Raft는 term 조건만으로 선출을 허용하면 **committed entry를 모르는 노드가 leader로 당선**되어
해당 entry를 overwrite할 수 있다. §5.4.1은 이를 구조적으로 차단한다.

---

## 데이터 유실 시나리오 (§5.4.1 없을 때)

```
Term 1:

  Node-A (Leader)
      │
      │  엔트리 X append
      │
      ├──► Node-B에 복제 완료  (X 있음)
      │
      ├──► Node-C에 복제 미완  (X 없음)
      │
      └── crash

  이 시점에 X는 A, B 2/3이 보유 → majority → committed

Term 2 선출 (§5.4.1 없음):

  Node-C: election timeout 먼저 발생
  Node-C: RequestVote(term=2, lastLogIndex=0, lastLogTerm=0) 전송

  Node-B: "term이 높으니까 투표" → grant ✅
  Node-C: 2/3 → Leader 당선

Term 2 진행:

  Node-C → Node-B에게 AppendEntries 전송
  Node-B의 엔트리 X가 Node-C의 빈 로그로 덮어씌워짐

  결과: committed entry X 영구 유실 💀
```

---

## §5.4.1 적용 후

```
Term 2 선출 (§5.4.1 있음):

  Node-C: RequestVote(lastLogIndex=0, lastLogTerm=0) 전송
  Node-B: candidateLogUpToDate(0, 0) 판정
            candidateLastLogTerm(0) < myLastLogTerm(1) → false → DENY ✅

  Node-C: 과반수 미달 → 선출 실패

  Node-B: election timeout → RequestVote(lastLogIndex=1, lastLogTerm=1)
  Node-C: candidateLogUpToDate(1, 1) 판정
            candidateLastLogTerm(1) > myLastLogTerm(0) → true → GRANT ✅

  Node-B: 2/3 → Leader 당선
  Node-B: 엔트리 X를 Node-C에 복제 → 데이터 보존 ✅
```

---

## 판정 규칙

> 더 최근 term의 로그가 이긴다. term이 같으면 더 긴 로그가 이긴다.

```go
// internal/infrastructure/raft/node.go
func (n *RaftNode) candidateLogUpToDate(candidateLastLogIndex, candidateLastLogTerm int64) bool {
    if candidateLastLogTerm != n.lastLogTerm {
        return candidateLastLogTerm > n.lastLogTerm
    }
    return candidateLastLogIndex >= n.lastLogIndex
}
```

term이 인덱스보다 우선하는 이유: 같은 term 내 로그는 prefix가 동일하다(§5.3 Log Matching
Property). term이 다를 때는 어느 쪽이 더 최신 리더의 후계자인지를 term 번호가 정확히 표현한다.

---

## 보장하는 불변식

committed entry는 **최소 majority 노드**가 보유한다.
candidate가 leader가 되려면 **majority의 투표**가 필요하다.
→ 두 집합의 교집합에는 반드시 committed entry를 가진 노드가 존재한다.
→ 그 노드는 §5.4.1에 의해 stale candidate의 vote를 거부한다.
→ stale candidate는 절대 leader가 될 수 없다.

---

## Phase 5b 연결 작업

현재(Phase 5a) `lastLogIndex/lastLogTerm`은 모두 0이므로 이 체크는 항상 통과한다.
Phase 5b에서 `HandleAppendEntries`가 실제 log entries를 append할 때 두 값을 업데이트해야
이 제한이 실질적으로 활성화된다.

`runCandidate`에서 아직 `peer.RequestVote(term, n.id, 0, 0)`으로 하드코딩되어 있음 —
Phase 5b에서 실제 `n.lastLogIndex`, `n.lastLogTerm` 값으로 교체 필요.

---

## 관련 테스트

`internal/infrastructure/raft/node_test.go`:

| 테스트 | 검증 내용 |
|--------|----------|
| `TestRaftNode_HandleRequestVote_StaleLogTerm_Denied` | candidateLastLogTerm < myLastLogTerm → deny |
| `TestRaftNode_HandleRequestVote_SameTermShorterLog_Denied` | 같은 term, 짧은 log → deny |
| `TestRaftNode_HandleRequestVote_SameTermEqualLog_Granted` | 동일 log → grant |
| `TestRaftNode_HandleRequestVote_NewerLogTerm_Granted` | candidateLastLogTerm > myLastLogTerm → grant (log 길이 무관) |
