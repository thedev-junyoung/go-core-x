# ADR-014: Raft KV State Machine — HTTP Propose + Apply-Wait

**상태**: Accepted
**날짜**: 2026-04-12
**관련**: [ADR-013](013-raft-write-path.md)

---

## 문제

Phase 5d에서 `Propose()` + `ApplyCh()`가 완성됐지만, 이를 외부에 노출하는 경로가 없었다.

구체적으로 세 가지가 빠져 있었다.

1. **KV 상태 머신**: 커밋된 로그 엔트리를 실제 key-value 저장소에 반영하는 소비자.
2. **HTTP 쓰기 경로**: 클라이언트가 Raft 클러스터에 KV 명령을 제출하는 진입점 (`POST /raft/kv`).
3. **선형적 읽기**: 쓰기 후 즉시 읽어도 반영된 값을 반환하는 `GET /raft/kv/{key}`.

---

## 결정

### KV 명령 인코딩

```go
type RaftKVCommand struct {
    Op    string `json:"op"`    // "set" | "del"
    Key   string `json:"key"`
    Value string `json:"value"` // empty for "del"
}
```

JSON 인코딩을 선택한 이유:
- 별도 스키마 파일 없이 디버깅/로깅이 용이하다.
- 이 프로젝트의 쓰기 경로에서 JSON 직렬화 비용은 무의미하다.
- Protobuf로 교체할 경우 `LogEntry.Data`의 바이트 슬라이스만 바꾸면 되므로 인터페이스가 안정적이다.

### KVStateMachine

```go
type KVStateMachine struct {
    mu      sync.RWMutex
    data    map[string]string
    waitMu  sync.Mutex
    waiters map[int64][]chan struct{}
}
```

- `Run(ctx, applyCh)` 고루틴이 `LogEntry`를 소비하고 `data` 맵에 반영한다.
- `WaitForIndex(ctx, index)` — 특정 로그 인덱스가 적용될 때까지 블로킹한다.
  HTTP 핸들러가 이를 호출하여 Propose → Apply 사이클이 완료됐음을 확인한다.

### HTTP 쓰기 경로 (POST /raft/kv)

```
요청 → JSON 디코딩
     → RaftNode.Propose(data)   // 리더가 아니면 503
     → KVStateMachine.WaitForIndex(index, 5s timeout)
     → 204 No Content
```

**동기 apply-wait**를 선택한 이유:

| 방식 | 장점 | 단점 |
|---|---|---|
| 202 Accepted (async) | 응답 빠름 | 클라이언트가 폴링해야 함; 오류 감지 어려움 |
| 204 + WaitForIndex (sync) | 쓰기 완료 보장, 단순한 클라이언트 | apply까지 레이턴시 포함 (50ms 이내) |

단일 노드 기준 HeartbeatInterval(50ms)에 commitIndex가 진전되므로 동기 대기 레이턴시는 평균 25ms 이하다. 클라이언트가 폴링하는 복잡도를 피하기 위해 동기 방식을 채택한다.

### Pending Waiter 정리

`WaitForIndex` 타임아웃 시 `waiters[index]` 슬라이스에서 해당 채널을 제거한다. 타임아웃된 요청이 나중에 apply되면 `notifyWaiters`가 이미 사라진 채널을 순회하지 않도록 방지한다.

### 라우트 등록 조건

```go
if raftNode != nil && raftKVSM != nil {
    mux.Handle("POST /raft/kv", ...)
    mux.Handle("GET /raft/kv/{key}", ...)
}
```

`CORE_X_NODE_ID`가 설정되지 않은 단일 노드 모드(Raft 비활성)에서는 라우트가 등록되지 않는다. 기존 `/ingest` + `/kv/{key}` 경로와 충돌 없이 공존한다.

---

## 기각된 대안들

### 기존 WAL KVStore에 Raft apply 통합

WAL KVStore는 `domain.Event`를 기록하는 Phase 2 구조체다. `RaftKVCommand`와 데이터 모델이 다르고 인코딩 형식도 다르다. 두 경로를 합치면 관심사가 섞인다. Raft 로그 자체가 영속성 계층이므로 별도 인메모리 맵으로 충분하다.

### sync.Cond로 apply 대기

`WaitForIndex`를 `sync.Cond.Wait()`으로 구현할 수 있다. 하지만 `Cond`는 Go의 context 취소(`ctx.Done()`)와 조합하기 어렵다 — 취소 시 `Cond.Wait`에서 깨어나려면 별도 goroutine이 필요하다. `chan struct{}`는 `select`와 자연스럽게 통합된다.

---

## 결과

| 속성 | 이전 | 이후 |
|---|---|---|
| Raft 쓰기 HTTP 진입점 | 없음 | `POST /raft/kv` (204 동기) |
| Raft KV 읽기 | 없음 | `GET /raft/kv/{key}` |
| Apply 완료 보장 | 없음 | `WaitForIndex` (5s timeout) |
| 상태 머신 | 없음 | `KVStateMachine` (in-memory map) |
