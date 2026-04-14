# Phase 8: Raft KV Durability — Implementation Spec

**버전**: 1.0
**날짜**: 2026-04-14
**ADR**: [ADR-016](../adr/016-raft-kv-durability.md)
**담당자**: core-x-forge-engineer (구현), chaos-auditor (검증)

---

## Goal

`KVStateMachine.apply()`가 커밋된 Raft 로그 엔트리를 Bitcask KVStore(`data/kv.wal`)에 영속화한다.
재시작 후 `lastAppliedIndex` 체크포인트를 기반으로 데이터 일관성을 복구한다.

**성공 기준**: `SIGKILL` 이후 프로세스 재시작 시 모든 committed KV 데이터가 복원된다.

---

## Architecture Constraints

### 불변 레이어 경계

- `internal/infrastructure/raft/` — KVStateMachine 수정 범위
- `internal/infrastructure/storage/kv/` — 새 Write 인터페이스 추가 범위
- `internal/infrastructure/storage/wal/` — 기존 코드 변경 최소화 (WAL 포맷 변경 금지)
- `internal/domain/` — 변경 금지 (domain.Event 구조 고정)

### 기존 성능 기준선 (Phase 1~2 측정값)

| 지표 | 기준값 | 출처 |
|---|---|---|
| WAL WriteEvent | ~10 μs (SyncNever 기준) | Phase 2 벤치마크 |
| HashIndex Get | ~50 ns | Phase 2 벤치마크 |
| WaitForIndex 평균 | ~25 ms | Phase 6 단일 노드 기준 |
| WaitForIndex p99 | <100 ms | Phase 7 3-노드 기준 |

Phase 8에서 WaitForIndex p99가 100ms를 초과하면 Sync 정책을 재검토한다.

### 외부 의존성 금지

ADR-001 결정: `go.mod`에 외부 라이브러리 추가 금지. 기존 `internal/infrastructure/storage/wal` 인프라만 사용한다.

### 쓰기 순서 (변경 불가)

```
Bitcask 쓰기 완료
  → lastApplied atomic 증가
    → notifyWaiters 호출
```

이 순서의 역전은 어떤 이유로도 허용되지 않는다.

---

## Implementation Steps

### Step 1: Bitcask KVStore에 KV 쓰기 인터페이스 추가

**파일**: `internal/infrastructure/storage/kv/store.go`

기존 `WriteEvent(e *domain.Event)` 외에 Raft KV 명령 전용 쓰기 메서드를 추가한다.
`domain.Event`는 Phase 1~2의 ingestion 경로 도메인 객체이므로 Raft KV 경로에서 재사용하지 않는다.

```go
// WriteKV는 Raft apply 경로에서 호출하는 key-value 쓰기 메서드.
// raftIndex는 체크포인트 추적을 위해 함께 저장된다.
func (s *Store) WriteKV(key, value string, raftIndex int64) error
```

내부 구현:
1. WAL 레코드 인코딩: `[type:1][raftIndex:8LE][keyLen:2LE][key:N][valueLen:2LE][value:M]`
2. `writer.Write(payload)` 호출 — SyncPolicy는 `SyncInterval(100ms)` 사용
3. `index.Set(key, offset)` 업데이트
4. `__raft_last_applied__` 메타 레코드를 동일 WAL에 append (별도 Write 호출)

`del` 명령 처리:
```go
func (s *Store) DeleteKV(key string, raftIndex int64) error
```
WAL에 타입 바이트로 `del` 표시 레코드를 append. 인덱스에서 해당 키 제거.

**주의**: `__raft_last_applied__` 메타 레코드는 WAL에서 예약된 키다. `WriteKV`/`DeleteKV` 인자로 이 키가 전달되면 `ErrReservedKey` 에러를 반환한다.

### Step 2: Bitcask KVStore Recover에 lastAppliedIndex 추출 추가

**파일**: `internal/infrastructure/storage/kv/store.go`

기존 `Recover(onRecover func(*domain.Event)) (int, error)` 는 Phase 1~2 ingestion 경로 복구용이다.
Raft KV 복구를 위한 별도 메서드를 추가한다.

```go
// RecoverKV는 Raft KV 데이터를 WAL에서 재생해 인덱스를 재구성하고
// 마지막으로 기록된 raftIndex를 반환한다.
func (s *Store) RecoverKV() (lastAppliedIndex int64, err error)
```

구현 세부:
- WAL을 순방향으로 재생한다.
- 레코드 타입이 `set`이면 `index.Set(key, offset)` 호출.
- 레코드 타입이 `del`이면 `index.Delete(key)` 호출.
- 레코드 타입이 `meta`(`__raft_last_applied__`)이면 `lastAppliedIndex` 갱신.
- 순방향 재생이므로 마지막 메타 레코드가 최종 `lastAppliedIndex`다.
- `ErrTruncated`는 정상(마지막 레코드 미완성) → 그 이전까지의 값 반환.
- `ErrCorrupted`는 fatal → 에러 반환.

### Step 3: KVStateMachine에 Bitcask Store 주입

**파일**: `internal/infrastructure/raft/kv_state_machine.go`

```go
type KVStateMachine struct {
    mu          sync.RWMutex
    data        map[string]string   // 인메모리 읽기 캐시 (유지)

    waitMu      sync.Mutex
    waiters     map[int64][]chan struct{}

    store       KVDurableStore      // 새로 추가: Bitcask 쓰기 인터페이스
    lastApplied atomic.Int64        // 새로 추가: 마지막 적용 인덱스
}

// KVDurableStore는 KVStateMachine이 의존하는 영속화 인터페이스.
// 인터페이스로 추출하는 이유: 테스트에서 MemKVDurableStore로 교체 가능.
type KVDurableStore interface {
    WriteKV(key, value string, raftIndex int64) error
    DeleteKV(key string, raftIndex int64) error
    GetKV(key string) (string, bool, error)
}
```

**`NewKVStateMachine` 시그니처 변경**:

```go
// store가 nil이면 인메모리 전용 모드 (테스트 하위 호환).
func NewKVStateMachine(store KVDurableStore) *KVStateMachine
```

기존 `NewKVStateMachine()` 호출부(`cmd/main.go`, `cluster_test.go`)를 업데이트한다.

### Step 4: apply() 메서드 수정

**파일**: `internal/infrastructure/raft/kv_state_machine.go`

```go
func (sm *KVStateMachine) apply(entry LogEntry) {
    var cmd RaftKVCommand
    if err := json.Unmarshal(entry.Data, &cmd); err != nil {
        slog.Warn("raft: kv state machine ignored malformed entry",
            "index", entry.Index, "err", err)
        // 파싱 실패: lastApplied는 올리지 않는다
        // 재시작 시 이 엔트리가 재시도되어 다시 실패할 것이나,
        // 로그 자체가 잘못된 것이므로 무한 루프는 아니다.
        sm.notifyWaiters(entry.Index)
        return
    }

    // Step 1: Bitcask 영속화 (store != nil 조건 확인)
    if sm.store != nil {
        var storeErr error
        switch cmd.Op {
        case "set":
            storeErr = sm.store.WriteKV(cmd.Key, cmd.Value, entry.Index)
        case "del":
            storeErr = sm.store.DeleteKV(cmd.Key, entry.Index)
        }
        if storeErr != nil {
            slog.Error("raft: kv durability write failed — lastApplied not advanced",
                "index", entry.Index, "op", cmd.Op, "err", storeErr)
            // lastApplied를 올리지 않는다. notifyWaiters도 호출하지 않는다.
            // HTTP 핸들러의 WaitForIndex는 타임아웃 후 에러를 반환한다.
            return
        }
    }

    // Step 2: 인메모리 캐시 업데이트
    sm.mu.Lock()
    switch cmd.Op {
    case "set":
        sm.data[cmd.Key] = cmd.Value
    case "del":
        delete(sm.data, cmd.Key)
    }
    sm.mu.Unlock()

    // Step 3: lastApplied 원자적 증가 (Bitcask 성공 후)
    sm.lastApplied.Store(entry.Index)

    slog.Debug("raft: kv applied", "op", cmd.Op, "key", cmd.Key, "index", entry.Index)
    sm.notifyWaiters(entry.Index)
}
```

### Step 5: 재시작 복구 메서드 추가

**파일**: `internal/infrastructure/raft/kv_state_machine.go`

```go
// RecoverFromStore는 재시작 시 Bitcask WAL을 재생해 인메모리 캐시를 복원하고
// lastAppliedIndex를 반환한다.
// raftLastApplied와 비교해 갭이 있으면 호출자(cmd/main.go)가 Raft 로그 재적용을 수행한다.
func (sm *KVStateMachine) RecoverFromStore() (lastAppliedIndex int64, err error)
```

구현:
1. `sm.store.RecoverKV()` 호출 → `bitcaskLastApplied` 획득
2. `sm.store`의 전체 KV 데이터를 `sm.data` 맵에 로드 (전체 인덱스 순회)
3. `sm.lastApplied.Store(bitcaskLastApplied)` 설정
4. `bitcaskLastApplied` 반환

**참고**: 전체 KV 데이터를 인메모리 맵에 로드하는 비용은 키 수 × 평균 Value 크기에 비례한다. Phase 8에서는 허용 가능하다고 가정한다. 키가 수백만 건을 초과하면 Phase 9에서 LRU 캐시로 교체한다.

### Step 6: cmd/main.go 재시작 복구 시퀀스 통합

**파일**: `cmd/main.go`

기존 Raft 초기화 순서에 복구 단계를 삽입한다.

```
// 기존: Raft WAL 로드 → RaftNode 생성 → KVStateMachine 생성 → Run
// 변경: Raft WAL 로드 → KVStateMachine.RecoverFromStore() → 갭 재적용 → Run

bitcaskLastApplied, err := raftKVSM.RecoverFromStore()
// err != nil → 치명적 오류, os.Exit(1)

raftEntries, err := logStore.LoadAll()
// raftLastApplied = len(raftEntries)에 해당하는 마지막 인덱스

if bitcaskLastApplied < raftLastApplied {
    // 갭 구간 재적용
    for _, entry := range raftEntries {
        if entry.Index > bitcaskLastApplied {
            raftKVSM.ApplyDirect(entry) // Run() 이전이므로 applyCh 없이 직접 호출
        }
    }
} else if bitcaskLastApplied > raftLastApplied {
    // 논리적으로 불가능한 상태
    slog.Error("FATAL: bitcask lastApplied ahead of raft log — data inconsistency",
        "bitcask", bitcaskLastApplied, "raft", raftLastApplied)
    os.Exit(1)
}
```

### Step 7: 인메모리 전용 테스트 헬퍼 구현

**파일**: `internal/infrastructure/raft/kv_state_machine_test.go` 또는 별도 파일

```go
// MemKVDurableStore는 테스트용 인메모리 KVDurableStore.
// 실제 WAL I/O 없이 KVStateMachine 로직을 단위 테스트할 수 있다.
type MemKVDurableStore struct {
    mu   sync.RWMutex
    data map[string]string
    lastApplied int64
}
```

기존 `cluster_test.go`의 `NewKVStateMachine()` 호출은 `NewKVStateMachine(nil)` 또는 `NewKVStateMachine(NewMemKVDurableStore())`으로 교체한다.

---

## Invariants

Chaos Auditor가 검증해야 할 불변 조건이다. 어떤 장애 시나리오에서도 이 조건이 깨지면 구현이 틀린 것이다.

### INV-1: Bitcask 선행 쓰기

```
Bitcask.WriteKV(key, value, index) 성공
  BEFORE
lastApplied.Store(index)
```

`lastApplied`가 N이라면 `bitcaskLastApplied >= N`이 보장된다.
역방향(lastApplied 증가 후 Bitcask 실패)은 절대 발생해선 안 된다.

### INV-2: 재시작 후 lastApplied 일치

```
프로세스 재시작 직후:
  assert bitcaskLastApplied <= raftLastApplied
재적용 완료 후:
  assert bitcaskLastApplied == raftLastApplied
```

### INV-3: 중복 apply 없음

```
entry.Index <= sm.lastApplied.Load()
  → apply() 호출 자체가 발생하지 않음
```

재시작 복구 시 `bitcaskLastApplied`보다 낮은 인덱스를 apply하면 중복이다. `ApplyDirect()`는 `entry.Index <= lastApplied`인 경우 no-op으로 처리해야 한다.

### INV-4: 인메모리 캐시와 Bitcask 일관성

```
정상 운전 중 임의 시점:
  sm.data[key] == Bitcask.GetKV(key)  (net 기준)
```

예외: apply() 실행 중 Step 1(Bitcask 쓰기) 완료 후 Step 2(인메모리 업데이트) 완료 전의 짧은 간격.
이 간격에서 Get()이 호출되면 이전 값을 반환한다. 이는 허용된 동작이다 (read-your-own-writes는 WaitForIndex로 보장).

### INV-5: Bitcask 쓰기 실패 시 notifyWaiters 미호출

```
Bitcask.WriteKV() → 에러
  → sm.lastApplied 변경 없음
  → notifyWaiters(entry.Index) 미호출
  → HTTP WaitForIndex → timeout 후 에러 반환
```

---

## Failure Scenarios

Forge Engineer가 방어해야 할 장애 시나리오다.

### FS-1: apply() 실행 중 SIGKILL (Bitcask 쓰기 전)

```
상황: Bitcask.WriteKV() 호출 전 프로세스 종료
기대: lastApplied가 증가하지 않았으므로
      재시작 시 bitcaskLastApplied < raftLastApplied
복구: Step 6의 갭 재적용 로직이 해당 엔트리를 재적용
      → 결과적으로 zero data loss
```

### FS-2: apply() 실행 중 SIGKILL (Bitcask 쓰기 후, lastApplied 증가 전)

```
상황: WriteKV 성공, atomic.Store(lastApplied) 미완료
기대: bitcaskLastApplied에 해당 인덱스가 기록되지 않을 수 있음
      (SyncInterval이라면 OS 버퍼에만 있을 수 있음)
복구 케이스 A (SyncInterval 범위 내): Raft 로그 재적용으로 복구
복구 케이스 B (SyncInterval 커밋 완료): WAL 재생 시 해당 레코드 발견
      → 중복 apply 발생 여부 확인 필요 (INV-3)
```

**Forge가 방어해야 할 것**: 중복 apply 시 KV 데이터가 idempotent하게 처리되는지. `set`은 멱등, `del`은 멱등. Phase 8에서는 안전하다.

### FS-3: Bitcask 디스크 풀 (ENOSPC)

```
상황: WriteKV() → ENOSPC 에러 반환
기대: apply()가 에러 로깅 후 return
      lastApplied 미증가, notifyWaiters 미호출
      HTTP 핸들러 → WaitForIndex 타임아웃 → 500 에러
결과: 이후 모든 쓰기 요청이 타임아웃됨
      운영자가 디스크 확보 후 프로세스 재시작 → 갭 재적용으로 복구
```

**Forge가 방어해야 할 것**: 디스크 풀 상태에서 Raft 선거/heartbeat는 계속 정상 동작해야 한다. KV 쓰기 실패가 Raft 합의 레이어로 전파되지 않아야 한다.

### FS-4: 재시작 시 bitcaskLastApplied > raftLastApplied

```
상황: 논리적으로 발생 불가하나 WAL 손상 등으로 발생 가능
기대: fail-fast (os.Exit(1))
      운영자 개입 필요 (로그 확인, 수동 복구)
```

**Forge가 방어해야 할 것**: 이 경우 자동 복구를 시도하지 않는다. 데이터 불일치를 조용히 덮어쓰면 더 큰 문제를 초래한다.

### FS-5: RecoverKV() 중 ErrCorrupted

```
상황: WAL 중간 레코드 체크섬 불일치 (디스크 비트 플립 등)
기대: RecoverFromStore() → 에러 반환 → os.Exit(1)
      부분 복구를 절대 허용하지 않는다
```

---

## Test Checklist

구현 완료 후 다음 테스트가 모두 통과해야 한다.

### 단위 테스트 (MemKVDurableStore 사용)

- [ ] `TestApply_WritesBeforeLastApplied`: Bitcask 쓰기 성공 후 lastApplied 증가 확인
- [ ] `TestApply_StoreFailure_NoLastAppliedAdvance`: WriteKV 실패 시 lastApplied 미증가
- [ ] `TestApply_StoreFailure_NoNotifyWaiters`: WriteKV 실패 시 WaitForIndex 타임아웃
- [ ] `TestApply_Idempotent_Set`: 동일 key에 set을 두 번 apply → 최종 값 일치
- [ ] `TestApply_Del`: del apply 후 Get → not found
- [ ] `TestRecoverFromStore_RebuildsCache`: RecoverKV 후 인메모리 맵 동일성 확인
- [ ] `TestRecoverFromStore_GapReapply`: bitcaskLastApplied < raftLastApplied → 갭 재적용 후 일치

### 통합 테스트 (실제 WAL 파일 사용)

- [ ] `TestDurability_SIGKILLBeforeWrite`: apply 중 강제 종료 → 재시작 후 데이터 복원
- [ ] `TestDurability_SIGKILLAfterWrite`: Bitcask 쓰기 후 lastApplied 전 종료 → 재시작 후 중복 없이 복원
- [ ] `TestDurability_DiskFull`: ENOSPC 주입 → WaitForIndex 타임아웃, Raft 계속 동작
- [ ] `TestDurability_CorruptedKVWAL`: WAL 체크섬 손상 → RecoverFromStore() 에러, os.Exit(1) 동작

### 성능 회귀 테스트

- [ ] `BenchmarkApply_WithStore`: SyncInterval(100ms) 설정에서 apply() throughput 측정
  - 기준: WaitForIndex p99 < 100ms 유지
- [ ] 기존 `BenchmarkWALWriter_*` 통과 확인 (WAL 인터페이스 변경 없음)

### 클러스터 통합 테스트 (기존 테스트 하위 호환)

- [ ] `TestCluster_ElectsLeader` — 변경 없이 통과
- [ ] `TestCluster_ProposeAndReplicate` — MemKVDurableStore 사용 시 변경 없이 통과
- [ ] `TestCluster_ProposeAndReplicate_Durable` — 실제 WAL 사용, 재시작 후 데이터 확인 (신규)

---

## Out of Scope

Phase 8에서 다루지 않는 항목이다. 이를 구현하면 scope creep이다.

| 항목 | 이유 | 예정 Phase |
|---|---|---|
| Raft 스냅샷 (`InstallSnapshot` RPC) | ADR-016 "기각된 대안" 참조 — 이 ADR의 성과물이 전제 조건 | Phase 9 |
| Raft 로그 컴팩션 (log truncation after snapshot) | 스냅샷 없이 로그 삭제 불가 | Phase 9 |
| KV 데이터 WAL SyncImmediate 격상 | 측정 전 변경 금지 (ADR-016 결정) | 벤치마크 후 결정 |
| 읽기 경로 선형화 (Linearizable Reads via ReadIndex) | 현재 GET은 인메모리 맵에서 바로 반환; stale read 허용 | Phase 10 |
| Bitcask WAL 자동 컴팩션 (GC) | ADR-006 기존 컴팩션 로직과 Raft index 통합 필요 | Phase 9 이후 |
| KV 데이터 암호화 | 보안 요구사항 미정의 | 미정 |
| 멀티 테넌시 / 네임스페이스 | 현재 단일 KV 공간 | 미정 |

---

*이 명세는 `core-x-forge-engineer`가 구현 착수 전 검토하고, `chaos-auditor`가 Invariants 및 Failure Scenarios 섹션을 기반으로 공격 계획을 수립한다.*
