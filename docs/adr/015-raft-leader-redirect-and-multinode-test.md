# ADR-015: Raft Leader Redirect + 멀티 노드 통합 테스트

**상태**: Accepted
**날짜**: 2026-04-12
**관련**: [ADR-014](014-raft-kv-state-machine.md)

---

## 문제

Phase 6에서 `POST /raft/kv`는 리더가 아닌 노드에 요청이 도달하면 503을 반환했다.
클라이언트가 직접 리더를 찾아야 하는 구조라 운영 복잡도가 높다.
또한 3-노드 클러스터의 end-to-end 동작을 검증하는 테스트가 없었다.

---

## 결정

### Leader Redirect (307 Temporary Redirect)

팔로워에 `POST /raft/kv`가 도달하면:

1. `RaftNode.LeaderID()` 로 현재 알려진 리더 ID를 조회한다.
2. `CORE_X_RAFT_HTTP_NODES` 환경 변수(`n1=http://host:8080,n2=...`)에서 해당 ID의 HTTP 주소를 찾는다.
3. 주소가 있으면 `307 Temporary Redirect` + `Location: {leader}/raft/kv` 반환.
4. 주소가 없거나 리더 ID를 모르면 기존대로 `503`.

#### leaderID 추적

`RaftNode` 에 `leaderID string` 필드를 추가했다.

| 이벤트 | leaderID 변화 |
|---|---|
| `HandleAppendEntries` 수락 | `args.LeaderID` 로 갱신 |
| `runLeader` 진입 | `n.id` (self) 로 갱신 |

Raft 논문 §5.1: "클라이언트가 팔로워에 요청을 보내면, 팔로워는 알려진 리더로 redirect한다."
307을 선택한 이유: 클라이언트가 POST body를 동일하게 재전송해야 함을 의미하며, 리더가 교체될 수 있으므로 영구 redirect(301/308)는 부적절하다.

#### 환경 변수 형식

```
CORE_X_RAFT_HTTP_NODES=n1=http://node1:8080,n2=http://node2:8081,n3=http://node3:8082
```

gRPC 주소(`CORE_X_PEERS`)와 HTTP 주소를 분리한 이유: 두 포트가 다르고 프로토콜도 다르기 때문이다. 단일 환경 변수로 합치면 파싱이 복잡해지고 관심사가 섞인다.

---

### 멀티 노드 통합 테스트 (`cluster_test.go`)

#### 설계: 실제 loopback gRPC 사용

| 방식 | 장점 | 단점 |
|---|---|---|
| 인메모리 fake transport | 빠름, 네트워크 없음 | Raft gRPC 스택을 우회해 실제 버그를 놓칠 수 있음 |
| 실제 loopback gRPC | 프로덕션 코드 경로 그대로 검증 | 약간 느림 (각 테스트 ~300ms) |

실제 loopback을 선택한 이유: 이 프로젝트의 gRPC transport 코드가 핵심 기능이므로, fake로 대체하면 회귀를 감지하지 못한다.

#### `startCluster(t, n)` 헬퍼

```
net.Listen(":0") × n     → 무작위 포트 확보
NewPeerClients(addrs)    → gRPC 게으른 다이얼 (서버 시작 전 OK)
grpc.NewServer() + Serve → goroutine에서 서버 시작
NewRaftNode + Run        → goroutine에서 Raft 실행
NewKVStateMachine + Run  → goroutine에서 상태 머신 실행
t.Cleanup                → ctx cancel + grpcSrv.Stop()
```

#### 테스트 목록

| 테스트 | 검증 내용 |
|---|---|
| `TestCluster_ElectsLeader` | 3-노드 클러스터가 5초 내 정확히 1명의 리더를 선출한다 |
| `TestCluster_ProposeAndReplicate` | 리더에 Propose → 3개 상태 머신 모두에 반영됨 |
| `TestCluster_LeaderIDKnownToFollowers` | 팔로워가 heartbeat 후 `LeaderID()` 를 올바르게 반환한다 |

---

## 기각된 대안들

### 팔로워가 자체적으로 요청을 리더에 forward

| | Redirect | Forward |
|---|---|---|
| 구현 복잡도 | 낮음 | 높음 (outbound HTTP + timeout 관리) |
| 네트워크 홉 | client→follower→client→leader | client→follower→leader |
| 오류 투명성 | 클라이언트가 직접 리더와 통신 | 팔로워 장애 시 연쇄 실패 가능 |

Redirect가 더 단순하고 장애 격리에 유리하다.

### 308 Permanent Redirect

리더는 언제든 교체될 수 있으므로 영구 redirect를 캐싱하면 잘못된 노드로 계속 요청이 간다. 307을 사용한다.

---

## 결과

| 속성 | 이전 | 이후 |
|---|---|---|
| 팔로워 요청 처리 | 503 | 307 → 리더 (주소 알 경우) / 503 유지 |
| leaderID 추적 | 없음 | `HandleAppendEntries` + `runLeader` 진입 시 갱신 |
| 클러스터 통합 테스트 | 없음 | 3-노드 loopback gRPC 테스트 3개 |
| 환경 변수 | — | `CORE_X_RAFT_HTTP_NODES` |
