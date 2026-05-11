# Core-X v3 — Concurrency and Observability

> v2가 Raft를 **올바르게 사용**하는 시간이었다면,
> v3는 **동시성 제어**와 **변경 전파**를 배우는 시간이다.

---

## v2가 남긴 gap

v2까지 Core-X는 다음을 보장한다:

- 쓰기: linearizable (Propose → WaitForIndex → 204)
- 읽기: linearizable (ReadIndex → WaitForIndex → Get)
- 멤버십 변경: split-brain 없음 (Joint Consensus)
- 단일 스토리지: 모든 쓰기가 Raft KVStateMachine 하나를 통과

**그런데 이런 문제는 아직 없다:**

```
문제 1 — 동시 쓰기 충돌
  client-A: GET key=x → "100"
  client-B: GET key=x → "100"
  client-A: SET key=x "90"  (100 - 10)
  client-B: SET key=x "80"  (100 - 20)  ← lost update. 실제로는 "70" 이어야 함
```

```
문제 2 — 변경 사항을 외부에서 구독할 방법이 없음
  다른 서비스가 "key=x가 변경됐을 때 알고 싶어"
  → Core-X는 polling 외에 방법을 제공하지 않음
  → Kafka, Debezium, Outbox 패턴이 해결하는 문제
```

---

## v3 목표

| Phase | 주제 | 핵심 문제 | DDIA |
|---|---|---|---|
| 13 | **MVCC** | 동시 쓰기의 lost update, dirty read | Ch.7 Transactions |
| 14 | **CDC** | 변경 사항의 외부 전파 | Ch.11 Stream Processing |

---

## Phase 13: MVCC (Multi-Version Concurrency Control)

### 문제

```
현재 KVStateMachine:
  sm.data = map[string]string{"x": "100"}
  SET x=90 → sm.data["x"] = "90"  (덮어쓰기, 이전 값 소멸)

동시 클라이언트:
  T1: READ x → "100"
  T2: READ x → "100"
  T1: WRITE x = "90"  (100 - 10)
  T2: WRITE x = "80"  (100 - 20)  ← lost update
  결과: "80" (T1의 변경 유실)
  기대: "70" (두 변경이 모두 반영)
```

### 해결: 버전 기반 다중 읽기

```
각 키에 값을 덮어쓰지 않고 버전을 추가한다:

  sm.versions["x"] = [
    {version: 1, value: "100", deleted: false},
    {version: 2, value: "90",  deleted: false},
    {version: 3, value: "80",  deleted: false},
  ]

읽기: "어느 버전을 볼 것인가?"
  - latest: 가장 최근 버전 (현재와 동일, ReadIndex로 linearizable)
  - snapshot(v=2): version 2 시점의 값 → "90"

쓰기: Compare-And-Swap (CAS)
  - "version=1일 때만 x를 90으로 바꿔라"
  - version이 다르면 → 409 Conflict → 클라이언트가 재시도
  - lost update 방지
```

### 실무 연결

- PostgreSQL의 `xmin` / `xmax`: 각 row에 생성/삭제 트랜잭션 ID 기록
- `SELECT ... FOR UPDATE` vs Optimistic Locking: 언제 어떤 걸 쓰나
- `REPEATABLE READ`: 트랜잭션 시작 시점의 snapshot을 끝까지 사용
- 재고 차감, 잔액 업데이트에서 왜 CAS가 필요한가

### 구현 범위

```go
// 현재
type KVStateMachine struct {
    data map[string]string
}

// v3
type MVCCStateMachine struct {
    versions map[string][]MVCCVersion  // key → sorted versions
    latest   map[string]uint64         // key → current version number
}

type MVCCVersion struct {
    Version uint64
    Value   string
    Deleted bool
}
```

- `RaftKVCommand`에 `ExpectedVersion uint64` 추가 (CAS 필드)
- `Op: "cas"` 추가: expected version 불일치 시 → 409
- `GET /kv/{key}?version=N` — 특정 버전 읽기 (snapshot read)
- version GC: 오래된 버전 정리 (configurable retention)

---

## Phase 14: CDC (Change Data Capture)

### 문제

```
현재: 쓰기가 KVStateMachine.apply()를 통과하면 외부에 알릴 방법이 없다

실무 시나리오:
  - 주문 서비스: "재고가 0이 되면 알림 발송"
  - 검색 서비스: "상품 정보가 변경되면 검색 인덱스 갱신"
  - 감사 로그: "누가 언제 무엇을 바꿨는지 기록"

현재 해결책: polling → 비효율, 지연, 놓칠 수 있음
```

### 해결: apply() 에서 change event 발행

```
Core-X WAL은 이미 모든 변경의 순서를 보장한다.
apply()가 실행될 때마다 → ChangeEvent를 구독자에게 전달한다.

apply(entry):
  1. 기존 로직 (durable write → in-memory update → lastApplied)
  2. changeLog.Publish(ChangeEvent{...})  ← CDC 추가 부분

구독자 (consumer):
  ch := cdc.Subscribe()
  for event := range ch {
      // event: {Type, Key, Value, Version, Timestamp, Offset}
  }
```

### 실무 연결

- MySQL binlog / PostgreSQL logical replication: WAL을 이벤트 스트림으로 노출
- Debezium: DB WAL을 읽어서 Kafka topic에 발행하는 CDC 도구
- Outbox 패턴: 트랜잭션 내 DB 쓰기 + 이벤트 발행을 원자적으로 (이중 쓰기 없이)
- Kafka consumer group: 여러 소비자가 offset 기반으로 병렬 소비

### 구현 범위

```go
type ChangeEvent struct {
    Type      string    // "set" | "del"
    Key       string
    Value     string
    Version   uint64    // MVCC version (Phase 13 연동)
    Offset    int64     // Raft log index (WAL offset)
    Timestamp time.Time
}

type ChangeLog struct {
    subscribers []chan ChangeEvent
    mu          sync.RWMutex
}
```

- `ChangeLog.Subscribe() <-chan ChangeEvent` — 구독 등록
- `ChangeLog.Unsubscribe(ch)` — 구독 해제 (goroutine 누수 방지)
- `GET /cdc/stream` — SSE(Server-Sent Events)로 HTTP 스트리밍
- offset replay: "offset=N부터 다시 받고 싶어" (WAL 재생)
- 통합 테스트: 쓰기 10건 → CDC 구독자가 순서대로 10건 수신 확인

---

## v3 학습 맵

| Phase | DDIA | 실무 도구 | 배우는 것 |
|---|---|---|---|
| 13 MVCC | Ch.7 §Lost Updates, §Snapshot Isolation | PostgreSQL row versioning, Optimistic Locking | 동시 쓰기 충돌 방지, CAS, 격리 수준 |
| 14 CDC  | Ch.11 §CDC, §Event Sourcing | MySQL binlog, Debezium, Kafka, Outbox 패턴 | 변경 전파, 이벤트 스트리밍, offset 재생 |

---

## 시작점

**Phase 13 (MVCC)** 가 먼저다.
CDC의 `ChangeEvent`에 version 필드가 있으므로 MVCC 먼저 구현하면 CDC와 자연스럽게 연결된다.

ADR-022 작성 → 구현 → CAS 충돌 시나리오 테스트 순서로 진행.
