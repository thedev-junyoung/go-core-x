# Phase 5a: Raft Leader Election - Architecture Guide

Core-X Phase 5a "Raft 리더 선출" 구현 문서입니다.

---

## 1. 왜 Raft인가 — split-brain 문제

Phase 3b까지의 상태: Primary 장애 시 수동으로 `CORE_X_ROLE=primary`를 재설정해야 한다.

단순한 대안 — **Heartbeat + 자동 승격**의 문제:

```
[정상]          Primary ──heartbeat──► Replica (OK)

[파티션]        Primary ─ ─ ─ ─ ─ ─ ┐ Replica
                                     │
               "나는 Primary야"        "Primary가 죽었다 → 나도 Primary야"
                     ↓                       ↓
               WAL에 계속 쓰기          WAL에 계속 쓰기
                                     ← split-brain! 두 노드의 데이터가 갈라짐 →
```

**Raft**: term 기반 리더 권한. 선출에서 이기려면 **과반수의 투표**가 필요하다. 두 노드가 동시에 과반수를 가질 수 없으므로 split-brain이 구조적으로 불가능하다.

| 방안 | split-brain 안전성 | 자동 장애조치 |
|------|-------------------|--------------|
| 수동 장애조치 | 해당 없음 | 없음 |
| Heartbeat + 자동 승격 | 없음 | 있음 |
| **Raft (채택)** | **있음** | **있음** |

---

## 2. Raft 선출 알고리즘 (§5.2)

### 2-1. 상태 머신

```
                  election timeout
Follower ──────────────────────────────► Candidate
   ▲                                         │
   │  높은 term 확인                          │ RequestVote 전송
   │  (즉시 Follower로)                       │
   │                                         │ 과반수 득표
   │                                         ▼
   └────────── heartbeat / 높은 term ──── Leader
```

모든 노드는 `Follower`로 시작한다. election timeout 시:
1. term을 1 증가
2. 자신에게 투표
3. 모든 피어에게 `RequestVote` 전송
4. 과반수 득표 → `Leader`로 전환, heartbeat 전송 시작

### 2-2. Term — Raft의 논리적 시간

더 높은 term의 메시지를 받으면 **즉시 Follower로** 돌아간다. 이것이 "더 최신 리더가 나타나면 현 리더는 물러난다"는 규칙의 구현이다.

```
Term 1: Node-A가 Leader
          ↓ 파티션 발생
Term 2: Node-B가 새 선출 → Leader
          ↓ 파티션 복구
Term 1의 Node-A가 Term 2 메시지 수신 → 즉시 Follower로 강등
```

### 2-3. Election Timeout 무작위화

모든 노드가 동시에 timeout되면 모두가 동시에 Candidate가 되어 split vote가 반복된다.

```
[무작위화 없음]  Node-A: 150ms timeout ┐
                Node-B: 150ms timeout ├── 동시 선출 → 모두 1표씩 → split vote 반복
                Node-C: 150ms timeout ┘

[무작위화]      Node-A: 163ms timeout  ← 가장 먼저 timeout → RequestVote 전송
                Node-B: 241ms timeout  ← 아직 Follower → Node-A에 투표
                Node-C: 287ms timeout  ← 아직 Follower → Node-A에 투표
                                         → Node-A가 과반수(2/3) 획득 → Leader!
```

| 파라미터 | 값 | 근거 |
|----------|----|------|
| `ElectionTimeoutMin` | 150ms | Raft 논문 하한값 |
| `ElectionTimeoutMax` | 300ms | Raft 논문 상한값 |
| `HeartbeatInterval` | 50ms | ElectionTimeoutMin보다 충분히 작아야 함 |
| RPC timeout | 100ms | ElectionTimeoutMin보다 작아야 함 |

`HeartbeatInterval(50ms) << ElectionTimeoutMin(150ms)` — 정상 상태의 Leader가 Follower의 election timer를 timeout 전에 리셋할 수 있다.

---

## 3. 코드 구조

### 3-1. 패키지 레이아웃

```
internal/infrastructure/raft/
├── state.go    RaftRole enum, 타이밍 상수
├── node.go     RaftNode — 상태 머신, 선출 루프, heartbeat 루프
├── client.go   RaftClient — 피어 RaftServer로의 outbound gRPC
└── server.go   RaftServer — inbound gRPC, RaftHandler 인터페이스에 위임
```

### 3-2. 상태 머신 구현 패턴

`Run()` loop가 역할별 함수를 순차 실행한다. 각 `runXxx()`는 역할 전환 시 단순히 `return`한다:

```go
func (n *RaftNode) Run(ctx context.Context) {
    for {
        switch n.role {
        case RoleFollower:  n.runFollower(ctx)
        case RoleCandidate: n.runCandidate(ctx)
        case RoleLeader:    n.runLeader(ctx)
        }
    }
}
```

goroutine 하나의 select loop 연쇄로 상태 머신을 표현하는 Go 관용 패턴이다.

### 3-3. `resetCh` — 비차단 채널 패턴

```go
resetCh: make(chan struct{}, 1),  // 버퍼 크기 1
```

heartbeat을 받으면 election timer를 리셋해야 한다. `HandleAppendEntries`는 gRPC goroutine에서 호출되고, 상태 머신 goroutine이 `resetCh`를 비우기 전에 여러 heartbeat이 도달할 수 있다:

```go
// gRPC goroutine에서
select {
case n.resetCh <- struct{}{}:
default:  // 채널이 차 있으면 skip — 이미 reset 신호가 대기 중
}
```

`default` 브랜치가 없으면 gRPC goroutine이 블록된다. 버퍼 1은 "reset이 필요하다"는 사실만 전달하면 충분하다.

### 3-4. 단일 노드 fast path

```go
majority := (len(peers)+1)/2 + 1  // peers=0이면 majority=1
// ...
if votes >= majority {  // 자신의 1표로 과반수 충족
    n.role = RoleLeader
    return
}
```

피어가 없으면 RequestVote RPC 없이 즉시 Leader가 된다.

### 3-5. `RaftHandler` 인터페이스 — 전송 계층 분리

```go
type RaftHandler interface {
    HandleAppendEntries(term int64, leaderID string) (currentTerm int64, success bool)
    HandleRequestVote(term int64, candidateID string, lastLogIndex, lastLogTerm int64) (currentTerm int64, voteGranted bool)
}

type RaftServer struct {
    pb.UnimplementedRaftServiceServer
    node RaftHandler  // *RaftNode가 아닌 인터페이스
}
```

`RaftNode`는 gRPC를 전혀 모른다. 테스트에서 실제 gRPC 서버 없이 `HandleAppendEntries`/`HandleRequestVote`를 직접 호출한다.

---

## 4. Proto 설계 — Phase 5b 하위 호환성 확보

Phase 5a에서 `RequestVoteRequest`에 Phase 5b용 필드를 미리 정의했다:

```protobuf
message RequestVoteRequest {
    int64  term           = 1;
    string candidate_id   = 2;
    int64  last_log_index = 3;  // Phase 5a에서는 0으로 전송
    int64  last_log_term  = 4;  // Phase 5b를 위해 미리 예약
}

// Phase 5a에서 heartbeat 전용 — entries 없음
message AppendEntriesRequest {
    int64  term      = 1;
    string leader_id = 2;
    // TODO(phase5b): entries, prev_log_index, prev_log_term, leader_commit 추가 예정
}
```

Phase 5b에서 proto 변경(wire format 변경) 없이 필드를 채우기만 하면 된다.

---

## 5. 두 개의 독립적인 역할 축

Phase 5a의 가장 중요한 설계 결정: **선출과 복제 역할을 의도적으로 분리**했다.

| 축 | 값 | 설정 방식 |
|----|-----|----------|
| 복제 역할 | Primary / Replica | `CORE_X_ROLE` 환경 변수 (정적) |
| Raft 역할 | Leader / Follower / Candidate | Raft 선출 (동적) |

```
[현재 Phase 5a]

  Node-A: CORE_X_ROLE=primary, Raft=Leader       ← 우연히 일치
  Node-B: CORE_X_ROLE=replica, Raft=Follower

  Node-A가 죽으면:
  → Raft: Node-B가 새 Leader로 선출됨 ✅
  → 복제: WAL 스트리밍은 여전히 CORE_X_ROLE=primary로 향함 ← 수동 변경 필요
```

왜 분리했는가: 선출 로직 + 역할 전환 + WAL 재배선을 한 번에 구현하면 동시에 세 가지를 디버깅해야 한다. 단계적으로 분리해서 각각 독립 검증한다.

Phase 5b에서 두 축이 연결된다: Raft Leader 선출 → 자동으로 복제 Primary 전환.

---

## 6. 선출 흐름 시퀀스

```
Node-A (Follower)       Node-B (Follower)       Node-C (Follower)
      │                       │                       │
      │  [150ms timeout]      │                       │
      │                       │                       │
      │── term=1, RequestVote ──────────────────────► │
      │── term=1, RequestVote ──────► │               │
      │                       │       │               │
      │◄─ vote_granted=true ──────── │               │
      │◄─ vote_granted=true ─────────────────────── │
      │                       │                       │
      │  votes=3 ≥ majority=2 │                       │
      │  → RoleLeader         │                       │
      │                       │                       │
      │── AppendEntries(term=1) ──────────────────── ►│
      │── AppendEntries(term=1) ───────► │            │
      │                       │  reset   │            │
      │                       │  timer   │            │
```

---

## 7. 현재 한계 (Phase 5b에서 해결)

| 한계 | 영향 | 해결 시점 |
|------|------|----------|
| term이 메모리에만 있음 | 재시작 시 term=0으로 초기화 | Phase 5b (WAL 영속화) |
| AppendEntries에 log entries 없음 | Raft가 실제 write를 조율하지 않음 | Phase 5b |
| Raft Leader ≠ 복제 Primary | 자동 failover 미완성 | Phase 5b |
| Follower read 미지원 | 모든 읽기가 Primary 집중 | Phase 5c (Read Index) |
