# ADR-016: Raft KV Durability — KVStateMachine을 Bitcask KVStore에 영속화

**상태**: Accepted
**날짜**: 2026-04-14
**관련**: [ADR-012](012-raft-log-wal-persistence.md), [ADR-013](013-raft-write-path.md), [ADR-014](014-raft-kv-state-machine.md)

---

## 문제

### 현재 구조의 내구성 결함

Phase 6~7에서 완성된 `KVStateMachine`은 커밋된 Raft 로그 엔트리를 인메모리 맵에만 반영한다.

```go
// 현재 코드 — 재시작 시 모든 데이터 소실
func (sm *KVStateMachine) apply(entry LogEntry) {
    sm.mu.Lock()
    switch cmd.Op {
    case "set":
        sm.data[cmd.Key] = cmd.Value  // in-memory only
    case "del":
        delete(sm.data, cmd.Key)      // in-memory only
    }
    sm.mu.Unlock()
    sm.notifyWaiters(entry.Index)
}
```

이 설계의 문제는 세 가지다.

**첫째, 재시작 시 데이터 소실.** 프로세스가 재시작되면 `data` 맵이 비워진다. Raft WAL(`data/raft_log.wal`)에는 로그 엔트리가 남아 있으나, 이를 재적용하려면 `lastApplied`를 0으로 초기화해야 한다. 단순히 로그 전체를 재적용하면 이미 처리한 명령을 중복 실행하는 문제가 생긴다.

**둘째, commitIndex와 KV 쓰기 간 원자성 부재.** 현재 `commitIndex`는 `RaftNode` 내부에서 관리되고, `KVStateMachine.apply()`는 별도 고루틴에서 비동기로 소비한다. 재시작 시 어느 엔트리까지 KV 스토어에 반영됐는지 추적하는 `lastAppliedIndex` 체크포인트가 없다.

**셋째, 재시작 일관성 복구 절차 미정의.** Raft WAL의 `lastApplied`와 Bitcask의 `lastAppliedIndex`가 일치하지 않을 때 어떻게 동기화하는지 정의되지 않았다.

**DDIA §11.1**: "WAL에 기록되지 않은 데이터는 기록되지 않은 것과 같다." — 인메모리 상태를 신뢰할 수 없다.

---

## 결정

### 1. KVStateMachine의 apply를 Bitcask KVStore에 영속화

`apply()` 메서드가 호출될 때 인메모리 맵 업데이트와 동시에 Bitcask WAL에 쓰기를 수행한다. 이때 순서는 반드시 다음과 같아야 한다.

```
1. Bitcask KVStore.Write(key, value) → WAL에 기록 (durable)
2. Bitcask에 성공한 이후에만 → lastApplied 증가
3. lastApplied 증가 이후에 → notifyWaiters(entry.Index)
```

이 순서를 강제하는 이유: `lastApplied`는 "이 인덱스까지 KV 스토어에 반영됐다"는 증거다. Bitcask 쓰기가 실패한 채로 `lastApplied`가 올라가면 재시작 후 해당 엔트리가 누락된다.

**중요**: 기존 `KVStateMachine`의 `data map[string]string`은 Phase 8 이후에도 읽기 캐시로 유지한다. `Get()`은 인메모리 맵에서 먼저 조회하고, Bitcask는 재시작 복구와 영속성 계층으로만 사용한다.

```go
// Phase 8 목표 구조 (pseudo-code)
func (sm *KVStateMachine) apply(entry LogEntry) {
    var cmd RaftKVCommand
    if err := json.Unmarshal(entry.Data, &cmd); err != nil {
        sm.notifyWaiters(entry.Index)
        return
    }

    // Step 1: Bitcask에 먼저 쓴다 (durable write)
    if err := sm.store.Write(cmd.Key, cmd.Value, entry.Index); err != nil {
        // 쓰기 실패: lastApplied를 올리지 않는다
        // 다음 재시작 시 이 엔트리를 다시 적용할 수 있다
        slog.Error("raft: kv durability write failed", "index", entry.Index, "err", err)
        return
    }

    // Step 2: 인메모리 맵 업데이트 (캐시)
    sm.mu.Lock()
    switch cmd.Op {
    case "set":
        sm.data[cmd.Key] = cmd.Value
    case "del":
        delete(sm.data, cmd.Key)
    }
    sm.mu.Unlock()

    // Step 3: lastApplied 증가 → 체크포인트 역할
    atomic.StoreInt64(&sm.lastApplied, entry.Index)

    // Step 4: HTTP 핸들러 대기 해제
    sm.notifyWaiters(entry.Index)
}
```

### 2. commitIndex 체크포인트: Bitcask 메타 레코드

재시작 시 "어디까지 적용했는가"를 알기 위해 `lastAppliedIndex`를 Bitcask WAL에 특수 메타 레코드로 저장한다. 이 레코드는 일반 KV 데이터와 동일한 WAL 포맷을 사용하되, 예약된 키(`__raft_last_applied__`)를 사용한다.

**원자성 보장 방식**: Bitcask WAL은 append-only이므로 `lastAppliedIndex` 업데이트는 "기존 값을 덮어쓰는" 방식이 아니라 "최신 레코드를 새로 append"하는 방식이다. `Recover()` 시 WAL을 순방향으로 재생하면 자연스럽게 마지막 값이 `lastAppliedIndex`가 된다.

```
WAL 레코드 예시:
[data] key="user:1" value="alice" raftIndex=42
[data] key="user:2" value="bob"   raftIndex=43
[meta] key="__raft_last_applied__" value="43"   ← 마지막 반영 인덱스
[data] key="user:3" value="carol" raftIndex=44
[meta] key="__raft_last_applied__" value="44"   ← 갱신됨
```

이 메타 레코드를 매 apply마다 쓰면 WAL 크기가 증가한다. 이 비용은 "체크포인트 없이 전체 Raft 로그를 재적용하는 비용"보다 작다.

**체크포인트 주기 선택지:**

| 방식 | 쓰기 부하 | 재시작 복구 시간 | 구현 복잡도 |
|---|---|---|---|
| 매 apply마다 | 높음 (N+1 쓰기) | 최소 | 낮음 |
| N번에 1번 (예: 100) | 낮음 | 최대 100 엔트리 재적용 | 중간 |
| 외부 스냅샷 | 없음 | 스냅샷 이후만 재적용 | 높음 (Raft §7) |

Phase 8에서는 **매 apply마다** 메타 레코드를 기록한다. 스냅샷 메커니즘은 Phase 9 이후로 미룬다. 이 결정은 재시작 복구 시간을 최소화하고 구현 복잡도를 낮추는 데 최적화된다.

### 3. Sync 정책 분석

Bitcask KVStore (`data/wal.log`)에 적용할 `SyncPolicy`를 결정해야 한다.

**현재 Raft WAL** (`data/raft_log.wal`)은 `SyncImmediate`다. 각 `Append()` 호출마다 fsync를 한다. Raft Safety §5.4: 로그 엔트리는 과반수 노드에 durable하게 저장된 이후에만 커밋된다.

**KV 데이터 WAL**에도 `SyncImmediate`가 필요한가?

```
Raft 관점에서의 내구성 체인:
  클라이언트 쓰기
    → Propose (리더)
    → Raft WAL fsync (SyncImmediate) ✓  ← 이미 durable
    → 과반수 복제
    → commitIndex 진전
    → applyCh
    → KV WAL 쓰기  ← 이 시점의 sync 정책이 논점
    → lastApplied 증가
```

핵심 통찰: Raft WAL에 이미 해당 엔트리가 durable하게 기록되어 있다. KV WAL 쓰기 직전에 프로세스가 죽더라도, 재시작 시 Raft WAL에서 해당 엔트리를 재적용할 수 있다. 따라서 **KV WAL은 `SyncImmediate` 없이도 논리적 내구성(logical durability)을 제공할 수 있다.**

그러나 실제 구현에서 두 가지 리스크가 있다.

- **리스크 A**: `SyncNever`로 설정 시 OS 크래시(OOM kill, kernel panic)에서 버퍼의 데이터가 소실될 수 있다.
- **리스크 B**: `SyncInterval`로 설정 시 간격 내 프로세스 크래시에서 간격 내 엔트리가 소실될 수 있다. 재시작 시 이 엔트리들을 Raft WAL로부터 재적용해야 한다.

**Phase 8 결정: `SyncInterval` (100ms)**

이유:
1. Raft WAL이 primary durability guarantee를 제공한다. KV WAL은 빠른 복구를 위한 보조 레이어다.
2. `SyncImmediate`는 매 apply마다 fsync → 50ms 레이턴시 증가 (디스크 I/O 대기). Raft heartbeat 주기(50ms)와 동일한 수준이라 WaitForIndex 레이턴시에 직접 영향을 준다.
3. 100ms 간격 내 OS 크래시가 발생해도 Raft 로그 재적용으로 복구된다. 이는 `lastAppliedIndex` 체크포인트가 올바르게 구현됐을 때의 보장이다.
4. 성능 기준: Phase 2에서 측정한 `WriteEvent` 평균 ~10μs는 `SyncNever` 기준이다. `SyncInterval(100ms)`는 이보다 약간 느리지만 측정 전 `SyncImmediate` 강제를 피한다.

### 4. 재시작 일관성 복구 절차

재시작 시 다음 순서로 복구를 수행한다.

```
Step 1: Bitcask WAL Recover()
  → 모든 KV 데이터 레코드 재생 → 인메모리 인덱스 재구성
  → __raft_last_applied__ 최신값 → bitcaskLastApplied 확인

Step 2: Raft WAL LoadAll()
  → 로그 엔트리 전체 로드
  → raftLastApplied = RaftNode.lastApplied (복구된 값)

Step 3: 갭 비교
  if bitcaskLastApplied < raftLastApplied:
      // KV WAL에 없는 엔트리가 Raft 로그에 있음
      // Raft 로그 [bitcaskLastApplied+1 .. raftLastApplied] 재적용
      for idx := bitcaskLastApplied + 1; idx <= raftLastApplied; idx++ {
          entry := raftLog[idx]
          sm.apply(entry)  // KV WAL 쓰기 + 인메모리 맵 업데이트
      }

Step 4: 일관성 확인
  assert bitcaskLastApplied == raftLastApplied
  // 이 조건이 깨지면: 데이터 불일치 → 재시작 실패 (fail-fast)
```

**엣지 케이스: `bitcaskLastApplied > raftLastApplied`**

이 경우는 이론적으로 발생해선 안 된다. Raft WAL fsync → 커밋 → applyCh → KV WAL 순서가 보장되기 때문이다. 그러나 발생하면 Raft 로그가 더 짧다는 의미이므로 데이터 불일치가 심각하다. 이 경우 프로세스를 즉시 종료하고 운영자가 개입하도록 한다 (fail-fast 원칙).

---

## 결과 (DDIA 3원칙 기준)

### Reliability (신뢰성)

- **개선**: 재시작 후 KV 데이터가 유지된다. 프로세스 크래시(not OS crash)에서 zero data loss.
- **개선**: `lastAppliedIndex` 체크포인트로 중복 apply가 방지된다.
- **위험**: `SyncInterval`을 선택했으므로 OS 크래시(kernel panic, OOM) 시 100ms 윈도우의 KV WAL 데이터 손실 가능. Raft 로그 재적용으로 복구 가능하나, 재시작 복구 시간이 늘어난다.

### Scalability (확장성)

- **영향 없음**: 단일 노드의 쓰기 경로에 Bitcask 쓰기가 추가된다. Phase 2에서 측정한 ~10μs per WriteEvent 기준, apply 레이턴시가 증가하지만 Raft heartbeat 대기 시간(평균 25ms)에 비해 무시할 수 있다.
- **체크포인트 WAL 증가**: 매 apply마다 메타 레코드 하나 추가. 1만 건의 KV 쓰기 시 추가 WAL 크기 ≈ 10,000 × (헤더 20B + "\_\_raft\_last\_applied\_\_" 22B + 8B index value) ≈ 500KB. 감내 가능한 수준이며, Phase 9 스냅샷 도입 시 압축된다.

### Maintainability (유지보수성)

- **개선**: `lastAppliedIndex`가 명시적으로 WAL에 기록되므로 "어디까지 적용됐는가"를 디버깅 시 직접 확인할 수 있다.
- **개선**: 재시작 복구 절차가 명문화됐다. 이전에는 "인메모리이므로 복구 없음"이었다.
- **복잡도 증가**: `apply()` 함수가 이전보다 복잡해진다. Bitcask 쓰기 실패 처리, 메타 레코드 쓰기, lastApplied 원자적 업데이트가 추가된다.

---

## 기각된 대안들

### Raft 로그 무한 보관 (Log Replay on Restart)

재시작 시 `lastApplied=0`으로 초기화하고 Raft WAL의 전체 로그를 처음부터 재적용한다.

**기각 이유:**

1. **복구 시간 O(N)**: 로그가 수백만 건이면 재시작에 분 단위가 걸린다. DDIA §8: "운영자는 수분간 서비스 불가를 허용하지 않는다."
2. **로그 압축 불가**: Raft §7에서 스냅샷(log compaction)을 도입해 오래된 로그를 버릴 수 있어야 한다. 로그 무한 보관은 이 메커니즘과 충돌한다.
3. **멱등성 보장 필요**: KV 명령(`set`, `del`)은 멱등하지만, 향후 `increment` 같은 비멱등 명령이 추가될 때 전체 설계를 바꿔야 한다.
4. **WAL 디스크 증가**: Raft 로그 WAL은 삭제되지 않으므로 무한히 증가한다. 운영 부담이 크다.

### 별도 스냅샷 파일 + 로그 압축 (Raft §7)

주기적으로 전체 상태를 스냅샷 파일에 덤프하고, 스냅샷 이전의 Raft 로그를 삭제한다.

**기각 이유 (Phase 8에서):**

1. **구현 범위 초과**: 스냅샷은 리더의 `InstallSnapshot` RPC, 팔로워의 스냅샷 수신/적용, 스냅샷 파일 관리까지 포함한다. Phase 8의 목표는 단순 내구성이다.
2. **선행 조건 미충족**: Bitcask `lastAppliedIndex` 체크포인트가 먼저 있어야 스냅샷 기점(snapshot index)을 올바르게 설정할 수 있다. 이 ADR의 결정이 스냅샷의 전제 조건이다.
3. **Phase 9로 연기**: Bitcask 영속화가 완성된 이후 스냅샷 도입이 자연스럽다.

### KV 데이터 WAL에 SyncImmediate 적용

매 apply마다 fsync를 강제하여 OS 크래시에서도 zero data loss를 보장한다.

**기각 이유:**

1. **이중 fsync 비용**: Raft WAL이 이미 `SyncImmediate`로 커밋마다 fsync한다. KV WAL까지 `SyncImmediate`를 적용하면 단일 쓰기 경로에 두 번의 fsync가 발생한다.
2. **WaitForIndex 레이턴시 직격**: 현재 WaitForIndex 평균 대기 ≈ 25ms. 여기에 KV WAL fsync ~5ms (HDD 기준)가 추가되면 p99 레이턴시가 의미 있게 증가한다.
3. **NVMe 환경 재측정 선행**: SSD/NVMe에서 fsync는 ~100μs 수준이다. 측정 없이 `SyncImmediate` 강제는 premature pessimization이다. Phase 8 이후 벤치마크로 실제 비용을 측정하고, 비용이 수용 가능하면 `SyncImmediate`로 격상한다.

---

## 모니터링 / 검증

Phase 8 구현 완료 후 다음 지표로 정상 동작을 검증한다.

```
지표 1: bitcaskLastApplied == raftLastApplied (재시작 후 즉시 확인)
지표 2: 재시작 복구 시간 < 1s (1만 건 기준)
지표 3: WaitForIndex p99 < 100ms (SyncInterval 도입 후 성능 회귀 없음)
지표 4: apply() 당 Bitcask 쓰기 실패율 = 0 (디스크 공간 부족 등 운영 지표)
```

테스트 시나리오:
- `SIGKILL` 직후 재시작: `lastAppliedIndex` 체크포인트에서 갭 없이 복구
- `SyncInterval` 윈도우 내 강제 종료: Raft 로그에서 갭 구간 재적용, 최종 상태 일치
- Bitcask 쓰기 실패 주입 (disk full 시뮬레이션): `lastApplied` 증가 없이 해당 엔트리 재시도 가능 상태 유지

---

## Related Decisions

- [ADR-002](0002-wal-design.md): Bitcask WAL 기본 설계 — append-only, SyncPolicy 인터페이스 정의
- [ADR-006](0006-wal-compaction.md): WAL 컴팩션 — Phase 9 스냅샷과 연계될 로그 압축 기반
- [ADR-012](012-raft-log-wal-persistence.md): Raft 로그 WAL 영속화 — `SyncImmediate` 선택 근거
- [ADR-013](013-raft-write-path.md): Raft 쓰기 경로 — Propose → applyCh 흐름
- [ADR-014](014-raft-kv-state-machine.md): KVStateMachine 설계 — 이 ADR이 교체하는 인메모리 맵 구조
