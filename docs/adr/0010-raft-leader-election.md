# ADR-010: Raft 리더 선출 (Phase 5a)

## 상태
채택됨

## 배경

ADR-008 (Phase 3b)에서 구현한 async WAL 스트리밍 복제는 데이터를 Primary에서 Replica로 내구적으로 복제하지만, 자동 장애조치가 없다:

- Primary가 죽으면 Replica가 데이터를 가지고 있지만 자동으로 쓰기를 서비스할 수 없다.
- 운영자가 Replica에서 `CORE_X_ROLE=primary`를 수동으로 설정하고 재시작해야 한다.
- 전체 쓰기 트래픽에 대한 단일 장애점(SPOF)이 존재한다.

Phase 5a는 **리더 선출** 문제만 해결한다. Raft를 통한 로그 복제(현재의 async WAL 스트림 교체)는 Phase 5b에서 진행한다.

## 결정

### 범위: 선출만 (Election Only)

Phase 5a는 Raft §5.2 (리더 선출)와 §5.4.1 (선출 제한)을 구현한다. Raft 로그 복제는 구현하지 **않는다**. ADR-008의 기존 async WAL 스트리밍은 변경하지 않는다.

Phase 5a의 `AppendEntries`는 **log entries를 포함하지 않는다** — heartbeat 전용 RPC다. `entries` 필드, `prev_log_index`, `prev_log_term`, `leader_commit`은 Phase 5b로 미룬다.

### 상태 머신

```
Follower  ──(election timeout)──►  Candidate  ──(과반수 득표)──►  Leader
   ▲                                    │                              │
   └───────(높은 term 확인)─────────────┘◄──────(heartbeat / 높은 term)─┘
```

모든 노드는 `Follower`로 시작한다. election timeout 시 Follower는 `Candidate`가 되어 term을 증가시키고 자신에게 투표한 뒤, 모든 피어에게 `RequestVote`를 보낸다. 과반수 득표 시 `Leader`가 되어 주기적으로 `AppendEntries` heartbeat을 전송한다.

### 신규 패키지: `internal/infrastructure/raft/`

| 파일 | 역할 |
|------|------|
| `state.go` | `RaftRole` enum (Follower/Candidate/Leader), 타이밍 상수 |
| `node.go` | `RaftNode` — 상태 머신, 선출 루프, heartbeat 루프 |
| `client.go` | `RaftClient` — 피어 RaftServer로의 outbound gRPC 호출 |
| `server.go` | `RaftServer` — inbound gRPC, `RaftHandler` 인터페이스에 위임 |

### Proto: RaftService

```protobuf
service RaftService {
  rpc RequestVote(RequestVoteRequest) returns (RequestVoteResponse);
  rpc AppendEntries(AppendEntriesRequest) returns (AppendEntriesResponse);
}

// RequestVoteRequest는 §5.4.1에 따라 last_log_index / last_log_term을 포함한다.
// Phase 5a에서는 두 값 모두 0으로 전송한다. 필드는 하위 호환성을 위해 미리 정의한다.
message RequestVoteRequest {
  int64  term           = 1;
  string candidate_id   = 2;
  int64  last_log_index = 3;
  int64  last_log_term  = 4;
}

message RequestVoteResponse {
  int64 term         = 1;
  bool  vote_granted = 2;
}

// AppendEntriesRequest는 Phase 5a에서 heartbeat 전용이다.
// TODO(phase5b): entries, prev_log_index, prev_log_term, leader_commit 추가 예정.
message AppendEntriesRequest {
  int64  term      = 1;
  string leader_id = 2;
}

message AppendEntriesResponse {
  int64 term    = 1;
  bool  success = 2;
}
```

### 타이밍

| 파라미터 | 값 | 근거 |
|----------|----|------|
| `ElectionTimeoutMin` | 150ms | Raft 논문 하한값 |
| `ElectionTimeoutMax` | 300ms | Raft 논문 상한값 |
| `HeartbeatInterval` | 50ms | ElectionTimeoutMin보다 충분히 작아야 함 |
| RPC timeout | 100ms | ElectionTimeoutMin보다 작아야 함 |

election timeout은 각 노드에서 `[150ms, 300ms)` 범위에서 무작위로 결정된다. 무작위화는 여러 Candidate가 동시에 선출을 시작할 때 발생하는 split vote를 방지한다 (Raft §5.2).

`HeartbeatInterval(50ms) << ElectionTimeoutMin(150ms)` 관계가 보장되어야 정상 상태의 Leader가 Follower의 election timer를 timeout 전에 리셋할 수 있다.

### `RaftHandler` 인터페이스

`RaftServer`(gRPC handler)는 구체적인 `*RaftNode`가 아닌 `RaftHandler` 인터페이스를 통해 호출한다. 이는 전송 계층과 상태 머신을 분리하여, gRPC 서버 없이 fake로 단위 테스트할 수 있게 한다.

```go
type RaftHandler interface {
    HandleAppendEntries(term int64, leaderID string) (currentTerm int64, success bool)
    HandleRequestVote(term int64, candidateID string, lastLogIndex, lastLogTerm int64) (currentTerm int64, voteGranted bool)
}
```

## 왜 Raft를 선택했는가

| 방안 | split-brain 안전성 | 자동 장애조치 | 복잡도 |
|------|-------------------|--------------|--------|
| 수동 장애조치 (현재) | 해당 없음 | 없음 | 최하 |
| Heartbeat + 자동 승격 | 없음 | 있음 | 낮음 |
| **Raft (채택)** | **있음** | **있음** | **중간** |

**수동 장애조치**는 현재 상태다. 장애 발생 시 운영자 개입이 필요하므로 프로덕션에서 허용할 수 없다.

**Heartbeat + 자동 승격**: "Primary가 응답 없으면 Replica가 스스로 승격"하는 단순한 방식이다. term 개념이 없으므로 네트워크 파티션이 복구된 후 두 노드가 모두 자신이 Primary라고 믿는 split-brain이 발생할 수 있다. 두 노드의 WAL 쓰기가 갈라지며 조정 메커니즘이 없다.

**Raft**: term 기반 리더 권한. 노드가 선출에서 이기려면 피어의 과반수로부터 표를 받아야 한다. 두 노드가 동시에 과반수를 가질 수 없으므로 split-brain이 구조적으로 불가능하다.

## 기존 역할(Role)과의 관계

Phase 5a는 **두 개의 독립적인 역할 축**을 도입한다:

| 축 | 값 | 설정 방식 |
|----|-----|----------|
| 복제 역할 | Primary / Replica | `CORE_X_ROLE` 환경 변수 (정적) |
| Raft 역할 | Leader / Follower / Candidate | Raft 선출 (동적) |

Phase 5a에서 두 축은 의도적으로 분리되어 있다. Raft Leader로 선출된 노드가 **자동으로 복제 Primary가 되지 않는다**. WAL 스트리밍은 여전히 ADR-008의 정적 `CORE_X_ROLE` 설정을 따른다.

Phase 5b에서 두 축을 연결한다: Raft Leader 선출 시 노드가 복제 역할을 Primary로 전환하고 Follower로 WAL 스트리밍을 시작한다.

## 결과

**긍정적 결과:**
- Primary 장애 시 자동 리더 선출 — 역할 승격을 위한 수동 운영자 개입 불필요
- Term 기반 split-brain 방지; Leader 선출에 과반수 쿼럼 필요
- `RaftHandler` 인터페이스로 gRPC 인프라 없이 노드 로직 단위 테스트 가능
- `RequestVoteRequest`에 `last_log_index` / `last_log_term` 필드를 미리 정의해둠으로써 Phase 5b 로그 복제 시 proto 변경 불필요

**부정적 결과:**
- Phase 5a는 선출과 복제를 연결하지 않는다. 새로 선출된 Raft Leader는 WAL 스트리밍에 영향을 주지 않는다 — 쓰기는 여전히 `CORE_X_ROLE=primary`인 노드로 향한다. Phase 5b 전까지 Primary 장애 시 복제 역할 승격은 수동 조작이 필요하다.
- Raft goroutine 루프가 노드마다 추가되어 baseline goroutine 수와 CPU wake-up(heartbeat 50ms마다)이 증가한다.

**Phase 5b로 미루는 항목:**
- log entries를 포함한 `AppendEntries`, `prev_log_index`, `prev_log_term`, `leader_commit`
- Raft commit index 추적
- Raft Leader 역할과 복제 Primary 역할 연결

**Phase 5c로 미루는 항목:**
- Read Index 기반 Follower read (Leader가 아닌 노드에서의 linearizable read)
