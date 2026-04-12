# ADR-011: Raft 메타데이터 영속화 및 역할 전환 전략 (Phase 5b)

## 상태

채택됨

## 배경

Phase 5a는 Raft 리더 선출(§5.2)과 선출 제한(§5.4.1)을 구현했다. Phase 5b로 넘어가기 전에
해결해야 할 구조적 문제가 두 가지 있다.

**문제 1 — 재시작 시 상태 소실**

Raft 논문 §5는 `currentTerm`과 `votedFor`를 *persistent state*로 분류한다: 노드는 크래시
이후에도 이 값을 잊어선 안 된다. 영속화가 없으면:

- 노드가 `term=0`으로 재시작한 뒤 `term=3` RequestVote를 받으면, 이미 이전 생에 행사한 투표를
  다시 허용할 수 있다 → §5.2 위반, 동일 term에서 중복 투표 가능.
- 재시작한 노드가 자신이 이미 초월한 stale leader의 heartbeat를 수락하는 잘못된 강등이 발생한다.

**문제 2 — 정적 복제 역할**

`cmd/main.go`가 시작 시 `CORE_X_ROLE` 환경 변수를 읽어 복제 역할을 정적으로 결정한다.
Raft가 새 리더를 선출해도 WAL 스트리밍 역할은 따라 바뀌지 않는다. Phase 5의 핵심 목표인
자동 장애조치가 미완성으로 남아 있다.

두 가지 설계 결정이 필요하다.

1. **Raft 메타데이터를 어디에, 어떻게 영속화할 것인가.**
2. **Raft 역할 변화를 복제 인프라에 어떻게 전달할 것인가.**

---

## 결정 1: raft_meta.bin — 고정 크기 바이너리 파일

Raft 메타데이터(`currentTerm`, `votedFor`)는 기존 WAL에 embed하지 않고, 별도의 고정 크기
바이너리 파일 **`raft_meta.bin`** 에 저장한다.

### 파일 포맷

```
[Version  : 1 byte ]
[Term     : 8 bytes] (int64, little-endian)
[VoteLen  : 1 byte ] (VotedFor 문자열 길이, 0 = 미투표)
[VotedFor : 255 bytes] (node ID, zero-padded)
[CRC32    : 4 bytes] (앞 265 bytes에 대한 체크섬)
─────────────────────
합계       : 269 bytes (고정)
```

offset 0에서의 단일 `pwrite(2)` 호출이 512-byte sector 내에서 atomic하게 전체 레코드를
덮어쓴다. CRC32가 torn write를 감지한다. 시작 시 파일 없음 → 초기 상태(`term=0`, `votedFor=""`)로 간주.

### 기각된 대안: WAL에 term/vote 레코드 embed

| 기준 | WAL embed | raft_meta.bin |
|---|---|---|
| 복구 비용 | O(WAL 크기) 전체 스캔 | O(1) 단일 read |
| I/O 결합도 | term 쓰기가 데이터 쓰기와 경합 | 독립된 I/O 경로 |
| Compaction | term 레코드가 compaction 대상에서 제외되어야 함 | 관련 없음 |
| 파일 크기 | 운영 환경에서 수십 GB | 269 bytes, 영구 고정 |

재시작마다 수십 GB WAL을 전체 스캔해서 최신 term 레코드를 찾는 것은 startup latency SLO상
허용 불가다. WAL compaction 로직이 "term 레코드는 절대 제거하지 않는다"는 미묘한 불변식을
유지해야 하는 문제도 있다.

### Save 호출 시점 (RPC 응답 전에 fsync 필수)

| 위치 | 저장 내용 | 이유 |
|---|---|---|
| `HandleRequestVote` — vote 부여 직전 | `{newTerm, candidateID}` | 응답이 candidate에 도달하기 전 크래시에도 vote 유지 |
| `HandleAppendEntries` — term 업데이트 시 | `{newTerm, ""}` | 재시작 후 stale heartbeat 수락 방지 |
| `runCandidate` — `currentTerm++` + self-vote 직후 | `{newTerm, nodeID}` | 같은 term에서 다른 candidate에게 재투표 방지 |

`MetaStore.Save` 실패는 하드 에러로 처리한다: RPC를 거부하여 불일치 상태 저장을 막는다.
Availability보다 Safety 우선.

### 인터페이스

```go
// RaftMeta는 §5에서 정의한 Raft persistent state를 담는다.
type RaftMeta struct {
    Term     int64
    VotedFor string // "" = 이번 term에서 미투표
}

// MetaStore는 무결성 보장과 함께 RaftMeta를 영속화하고 복구한다.
type MetaStore interface {
    Load() (RaftMeta, error) // 시작 시 Run() 호출 전에 1회 호출
    Save(m RaftMeta) error   // 상태 전환마다 호출; 반환 전 내구성 보장 필수
    Close() error
}
```

`MetaStore`는 `RaftNode`에 주입된다. `nil`이면 영속화 비활성(기존 테스트는 수정 없이 통과).

---

## 결정 2: RoleController — Callback 대신 Polling

복제 역할 전환은 `RaftNode.Role()`을 50ms 주기로 polling하는 `RoleController`가 담당한다.
`RaftNode` 내부에 callback을 등록하는 방식은 채택하지 않는다.

### 설계

```go
// RoleController는 Raft 역할 변화를 감지하여 ReplicationManager에 위임한다.
// 전용 goroutine 1개에서 동작한다.
type RoleController struct {
    raft        RaftRoleObserver   // RaftNode의 부분 인터페이스: Role() + Term()
    replication ReplicationManager // BecomeLeader / BecomeFollower
    interval    time.Duration      // 기본값 50ms
}

func (rc *RoleController) Run(ctx context.Context) {
    ticker := time.NewTicker(rc.interval)
    defer ticker.Stop()
    current := raft.RoleFollower
    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            next := rc.raft.Role()
            if next == current {
                continue
            }
            switch next {
            case raft.RoleLeader:
                rc.replication.BecomeLeader(ctx)
            default:
                rc.replication.BecomeFollower(ctx)
            }
            current = next
        }
    }
}
```

### 기각된 대안: RaftNode 내부 Callback 등록

Callback 방식은 `runCandidate`나 `runLeader` 내부에서 `onBecomeLeader()` /
`onBecomeFollower()`를 직접 호출한다. 이 함수들은 `RaftNode.mu`를 보유한 상태에서 실행된다.

위험:

- **데드락**: `ReplicationManager.BecomeLeader`가 gRPC 스트리밍을 시작하고, 이 과정에서
  자신의 mutex를 획득하거나 `RaftNode.mu`를 다시 획득하려는 재진입 호출이 발생할 수 있다.
- **계층 위반**: `RaftNode`(합의 계층)가 복제 인프라(전송 계층)를 직접 참조 → 의존성 방향
  역전.
- **테스트 복잡도**: `RaftNode` 단위 테스트에 복제 callback stub이 필요해진다.

### Polling 트레이드오프

| 속성 | Polling (채택) | Callback |
|---|---|---|
| 반응 지연 | ≤ 50ms | < 1ms |
| 데드락 위험 | 없음 (mu 해제 후 poll) | 있음 |
| 계층 결합도 | 없음 (`RaftNode`는 `RoleController` 무관) | 강함 |
| 테스트 격리 | 독립 단위 테스트 가능 | 공유 mock 필요 |

50ms 반응 지연은 허용 가능하다: HeartbeatInterval이 이미 50ms이므로, 새 리더가 첫 heartbeat를
보내는 시간 내에 복제 전환이 완료된다. 쓰기 가용성에 실질적 영향 없음.

### ReplicationManager 인터페이스

```go
type ReplicationManager interface {
    BecomeLeader(ctx context.Context) error   // WAL Streamer 시작, gRPC service 등록
    BecomeFollower(ctx context.Context) error // Streamer 종료, 새 primary에 연결
    BecomeStandalone() error                  // 모든 복제 중단 (단일 노드 모드)
}
```

**gRPC hot-swap 제한**: `grpc.Server`는 `Serve()` 이후 handler 재등록을 지원하지 않는다.
해결책: `ReplicationServer`는 항상 등록해두되, 내부 atomic flag로 요청 처리를 제어한다.
비활성 상태의 `StreamWAL` 호출은 `codes.Unavailable`을 반환한다 — **null-object 패턴**.

---

## 결정 3: §5.3 Log Matching Property 구현 전략

Phase 5b Steps 4–9에서 Raft §5.3 Log Matching을 구현했다. 목표는 리더가 팔로워의 로그를
자신의 것과 일치시키도록 강제하는 것이다.

### 3.1 Proto 확장 (Step 4–5)

`AppendEntriesRequest`와 `AppendEntriesResponse`에 §5.3 필드를 추가했다.

```protobuf
// AppendEntriesRequest — 기존 heartbeat 필드에 §5.3 필드 추가
message AppendEntriesRequest {
  int64  term           = 1;
  string leader_id      = 2;
  int64  prev_log_index = 3; // 새로 추가: 새 엔트리 직전 인덱스
  int64  prev_log_term  = 4; // 새로 추가: prevLogIndex 엔트리의 term
  repeated LogEntry entries = 5; // 새로 추가: 복제할 로그 엔트리 목록
  int64  leader_commit  = 6; // 새로 추가: 리더의 commitIndex
}

// AppendEntriesResponse — Fast Backup을 위한 conflict 필드 추가
message AppendEntriesResponse {
  int64 term           = 1;
  bool  success        = 2;
  int64 conflict_index = 3; // 새로 추가: 충돌 term의 첫 번째 인덱스
  int64 conflict_term  = 4; // 새로 추가: 충돌 엔트리의 term (0 = prevLogIndex에 엔트리 없음)
}

// LogEntry — §5.3 로그 레코드
message LogEntry {
  int64 index = 1; // 1-based Raft 로그 인덱스
  int64 term  = 2; // 리더가 수신한 term
  bytes data  = 3; // 불투명한 커맨드 페이로드
}
```

### 3.2 인메모리 로그 자료구조 (Step 6–7)

WAL을 §5.3 구현 전에 변경하면 포맷 의존성이 생긴다. Phase 5c에서 WAL을 연동하기 전까지
인메모리 슬라이스로 관리한다.

```go
// RaftNode에 추가된 필드
log         []LogEntry    // 0-indexed; log[i].Index == i+1 (1-based Raft 인덱스)
commitIndex int64         // quorum에 의해 커밋된 가장 높은 인덱스
lastApplied int64         // 상태 머신에 적용된 가장 높은 인덱스 (Phase 5b에서 미사용)

// Leader-only 휘발성 상태 (§5.3). peer 주소를 키로 사용.
// 리더 당선 시 lastLogIndex+1로 초기화; term마다 리셋.
nextIndex  map[string]int64 // 각 팔로워에게 보낼 다음 인덱스 (낙관적)
matchIndex map[string]int64 // 각 팔로워에서 복제 확인된 가장 높은 인덱스 (비관적)
```

리더 당선 시 초기화 규칙:
- `nextIndex[peer] = lastLogIndex + 1` (낙관적 — 팔로워가 완전히 최신이라고 가정)
- `matchIndex[peer] = 0` (비관적 — 아무것도 확인되지 않음)

### 3.3 Fast Backup 최적화

순진한 구현은 `nextIndex`를 1씩 감소시켜 리더-팔로워 로그 수렴에 O(n) 라운드트립이 필요하다.
Fast Backup은 충돌 정보를 활용해 O(1) skip을 달성한다.

**팔로워 측 (`HandleAppendEntries`):**

| 상황 | conflictIndex | conflictTerm | 의미 |
|---|---|---|---|
| prevLogIndex에 엔트리 없음 | `lastIdx + 1` | `0` | 팔로워 로그 길이로 점프 |
| term 불일치 | 충돌 term의 첫 번째 인덱스 | 충돌 term | 리더가 해당 term 전체를 건너뛸 수 있음 |

**리더 측 (`sendHeartbeats`):**

```
conflictTerm == 0:
  → nextIndex[peer] = conflictIndex  (팔로워가 prevLogIndex에 엔트리 없음)

conflictTerm > 0:
  → 리더 로그에서 term == conflictTerm인 마지막 인덱스 탐색 (후방 탐색, 보통 O(1))
  → 찾으면: nextIndex = 리더의 마지막 conflictTerm 인덱스 + 1
  → 못 찾으면: nextIndex = conflictIndex
```

팔로워가 `conflictTerm`에 해당하는 모든 엔트리를 한 번의 라운드트립으로 건너뛰게 되므로,
장기간 분리된 노드의 수렴 속도가 크게 향상된다.

### 3.4 §5.4.2 Current Term Only Commit 규칙

리더는 이전 term의 엔트리만으로 quorum을 형성해도 commitIndex를 올리지 않는다.
Raft 논문 Figure 8의 시나리오를 방지하기 위해, **현재 term의 엔트리**가 quorum에 복제된 경우에만
commitIndex를 올린다.

```go
// maybeAdvanceCommitIndex: N > commitIndex, quorum matchIndex[peer] ≥ N,
// log[N].term == currentTerm 세 조건이 모두 충족되어야 commitIndex = N
func (n *RaftNode) maybeAdvanceCommitIndex(currentTerm, lastIdx int64, majority int) {
    for N := lastIdx; N > n.commitIndex; N-- {
        e, ok := n.entryAt(N)
        if !ok || e.Term != currentTerm {
            continue // 이전 term 엔트리는 skip
        }
        count := 1 // self
        for _, m := range n.matchIndex {
            if m >= N { count++ }
        }
        if count >= majority {
            n.commitIndex = N
            break
        }
    }
}
```

### 3.5 기각된 대안

| 대안 | 기각 이유 |
|---|---|
| WAL 직접 복제 (Raft 엔트리 = WAL 레코드) | WAL 포맷이 §5.3 구현 전에 변경되어야 함 → 단계 분리 불가, 단일 PR이 너무 커짐 |
| nextIndex 단순 감소 (naive) | 장기 분리 노드 수렴 시 O(n) 라운드트립 → 불필요한 레이턴시 |
| commitIndex Follower 즉시 적용 | `lastApplied` 없이 적용하면 Phase 5c 상태 머신 연동 시 중복 적용 위험 |

### 3.6 §5.3 구현 단계 (Steps 4–9)

| 단계 | 대상 파일 | 변경 내용 |
|---|---|---|
| 4 | `proto/ingest.proto` | `AppendEntriesRequest` 필드 추가, `LogEntry` 메시지 추가, `AppendEntriesResponse` Fast Backup 필드 추가 |
| 5 | `proto/pb/*.go` | `make proto` 재생성 |
| 6 | `internal/infrastructure/raft/server.go` | `RaftHandler` 인터페이스에 `AppendEntriesArgs` 타입 반영 |
| 7 | `internal/infrastructure/raft/node.go` | `log[]`, `commitIndex`, `nextIndex/matchIndex`, `HandleAppendEntries` §5.3 로직, `sendHeartbeats` 엔트리 전송, `maybeAdvanceCommitIndex` |
| 8 | `internal/infrastructure/raft/client.go` | `AppendEntries` 호출 시그니처에 `AppendEntriesArgs` 전달 |
| 9 | `internal/infrastructure/raft/node_test.go` | §5.3 Log Matching 테스트 (prevLogIndex 검증, Fast Backup, commitIndex 전파) |

---

## 결과

**긍정적 효과**

- `RaftNode`는 복제 인프라에 대한 의존성이 전혀 없는 순수 합의 상태 머신으로 유지된다.
- WAL 크기와 무관하게 재시작 복구가 O(1)이다.
- 기존 단위 테스트는 수정 없이 통과한다 (MetaStore nil-safe, RoleController 독립 테스트 가능).
- Phase 5b의 세 하위 단계를 독립적으로 머지할 수 있다: 영속화 → 로그 복제 → 역할 전환.

**부정적 효과 / 위험**

- `raft_meta.bin`이 새로운 운영 산출물로 추가된다. WAL이 있는데 이 파일이 없으면 term이
  0으로 초기화되어 회복 가능하지만 혼란을 줄 수 있다. 백업·복구 절차에 반드시 포함해야 한다.
- 50ms polling이 노드 생명주기 동안 goroutine 1개를 상시 유지한다. CPU 비용은 무시할 수준.
- `RaftNode` 생성자에 `MetaStore` 파라미터가 추가되어 `cmd/main.go`와 `RaftNode`를 직접
  생성하는 테스트들을 업데이트해야 한다.

---

## 구현 계획 (Phase 5b 단계별)

| 단계 | 대상 파일 | 변경 유형 |
|---|---|---|
| 1 | `internal/infrastructure/raft/meta_store.go` | 신규 |
| 2 | `internal/infrastructure/raft/node.go` | MetaStore 주입, Save 호출 3곳 추가 |
| 3 | `internal/infrastructure/raft/node_test.go` | 영속화 라운드트립 테스트 |
| 4 | `proto/ingest.proto` | AppendEntries 확장, LogEntry 추가 |
| 5 | proto 생성 파일 | `make proto` 재생성 |
| 6 | `internal/infrastructure/raft/server.go` | RaftHandler 인터페이스 + server 업데이트 |
| 7 | `internal/infrastructure/raft/node.go` | log[], commitIndex, nextIndex/matchIndex 추가 |
| 8 | `internal/infrastructure/raft/client.go` | AppendEntries 시그니처 확장 |
| 9 | `internal/infrastructure/raft/node_test.go` | §5.3 Log Matching 테스트 |
| 10 | `internal/infrastructure/replication/manager.go` | 신규 |
| 11 | `internal/infrastructure/cluster/role_controller.go` | 신규 |
| 12 | `cmd/main.go` | 정적 CORE_X_ROLE 배선 → RoleController 전환 |

단계 1–3은 독립적으로 리뷰·머지 가능.
단계 4–9는 인터페이스 경계 변경으로 인해 단일 PR로 묶어야 한다.
단계 10–12는 안정적인 Raft 로그 복제가 완성된 후 진행한다.
