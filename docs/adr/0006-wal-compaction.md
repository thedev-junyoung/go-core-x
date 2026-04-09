# ADR-006: WAL Compaction — Stop-the-World Strategy

- **Status**: Accepted
- **Date**: 2026-04-09
- **Deciders**: Core-X Principal Architect

---

## Context

### 문제: append-only WAL의 파일 크기 증가

Bitcask 모델(ADR-005)에서 WAL은 append-only로 기록된다. 같은 source의 이벤트가 여러 번 기록되면 이전 버전의 레코드가 파일에 누적된다.

```
source="user-a" → v1 기록 (offset=100)
source="user-a" → v2 기록 (offset=200)  ← index가 이것만 가리킴
source="user-a" → v3 기록 (offset=300)  ← index가 이것만 가리킴

WAL 파일: 3개 레코드
실제 live 레코드: 1개 (v3)
공간 낭비: 2개 레코드 (67%)
```

업데이트가 빈번한 워크로드에서 WAL이 무한 증가하고, Recover() 시간도 O(전체 파일 크기)로 증가한다.

### 목표

- Live key당 최신 레코드만 남기고 stale 레코드 제거
- 기존 읽기/쓰기 인터페이스 무변경
- Crash 후에도 정합성 유지

---

## Decision

### Stop-the-World Compaction

WAL 전체를 새 파일에 재기록하는 동안 쓰기를 일시 중단한다.

**절차:**

```
Compact() 호출
  │
  ├─ writer.RunExclusiveSwap(fn) 획득
  │    ↓ (writer.mu Lock — 쓰기 차단)
  │
  ├─ 1. index.Snapshot()          [live key → offset 복사]
  │
  ├─ 2. writeCompactedWAL(tempPath, snapshot, readFile)
  │       └─ 각 live key: ReadRecordAt(old offset) → WriteEventOffset(new file)
  │
  ├─ 3. w.Sync()                  [batched fsync]
  │
  ├─ 4. os.Rename(tempPath, walPath)  [atomic on POSIX]
  │
  ├─ 5. readFile 교체              [새 파일 핸들 오픈]
  │
  ├─ 6. index 재구축               [새 offset으로 HashIndex 재생성]
  │       └─ store.mu.Lock()으로 (index, readFile) 쌍 원자 교체
  │
  └─ 7. writer file 교체           [RunExclusiveSwap 반환값]
       ↑ (writer.mu Unlock — 쓰기 재개)
```

### 핵심 설계 결정

#### 1. writer.RunExclusiveSwap: 쓰기 차단 + file 교체

```go
func (w *Writer) RunExclusiveSwap(fn func() (*os.File, int64, error)) error {
    w.mu.Lock()
    defer w.mu.Unlock()

    newFile, newSize, err := fn()
    if err != nil {
        return err
    }

    old := w.file
    w.file = newFile
    w.sizeTracker.Store(newSize)
    return old.Close()
}
```

Writer의 내부 lock을 잡은 채로 fn을 실행하여 compaction 전 기간 동안 쓰기를 직렬화한다. fn이 반환한 새 file handle과 size로 writer 상태를 원자적으로 교체한다.

**왜 writer.mu를 직접 노출하지 않는가?**
lock을 Store에서 잡으면 writer 내부 상태(file, sizeTracker)와 외부 교체 로직이 분리되어 일관성 보장이 어렵다. RunExclusiveSwap은 "lock 잡기 + 상태 교체"를 하나의 원자적 오퍼레이션으로 캡슐화한다.

#### 2. store.mu.RWMutex: Get과 Compact의 pointer race 방지

```
Compact():                          Get():
  store.mu.Lock()                     store.mu.RLock()
  store.readFile = newReadFile         rf := store.readFile  ← 어느 버전?
  store.index = newIndex              store.mu.RUnlock()
  store.mu.Unlock()
```

Compact가 `(index, readFile)` 쌍을 교체하는 동안 Get이 구 index + 새 readFile 조합을 볼 수 있다. store.mu로 교체를 원자화하여 항상 일관된 쌍을 읽도록 보장한다.

#### 3. os.Rename: 충돌 안전 파일 교체

```
Before rename:
  wal.data      ← 원본 (untouched)
  wal.data.compact ← 새로 쓴 파일

os.Rename("wal.data.compact", "wal.data")
  ← POSIX: 단일 syscall, atomic

After rename:
  wal.data      ← compacted (새 파일)
```

rename 전에 crash나면 원본 WAL은 그대로이고 `.compact` 파일만 남는다. 다음 Compact() 호출 시 orphan을 제거하고 재시도한다.

rename 후에 crash나면 compacted WAL이 `wal.data`에 있고, Recover()가 올바르게 재생한다.

#### 4. Batched fsync

```go
// writeCompactedWAL 내부
w := wal.NewWriter(wal.Config{SyncPolicy: wal.SyncNever})
// ... 모든 레코드 기록 ...
w.Sync()  // 마지막에 한 번만 fsync
```

레코드마다 fsync하면 I/O 횟수가 live key 수에 비례한다. 전체 기록 후 단일 fsync는 durability를 동일하게 보장하면서 I/O를 1회로 줄인다.

---

## Consequences

### 성능 특성

```
Compact() 지속 시간 = O(unique live key 수 × 평균 레코드 크기)

예: 10,000 live key, 평균 레코드 128 bytes
  writeCompactedWAL: 10,000 * ~1 μs = ~10 ms
  fsync: ~1-5 ms
  총합: ~15 ms pause

예: 1,000,000 live key, 평균 레코드 128 bytes
  총합: ~1.5 sec pause
```

쓰기 일시 중단 시간이 live key 수에 비례한다. 트래픽이 높은 환경에서는 호출 시점을 저트래픽 시간대로 제한해야 한다.

### Crash Safety

| 시점 | 결과 |
|------|------|
| rename 이전 crash | 원본 WAL 무손상, .compact orphan은 다음 Compact()에서 제거 |
| rename 이후 crash | compacted WAL에서 Recover() 정상 동작 |
| writer file 교체 중 crash | rename은 완료됐으나 writer file handle이 구 inode를 가리킴 — 재시작 시 NewWriter가 새 파일 열어서 해결 |

### 단점 및 제약

**Stop-the-World pause**
쓰기가 차단된다. 고처리량 환경에서는 pause가 SLA를 위반할 수 있다.

개선 방향 (Background Compaction):
```
1. lock 없이 temp 파일 기록 (읽기만 필요, 쓰기와 독립)
2. 기록 완료 후 짧은 시간만 lock 잡고 rename + pointer swap
3. pause ≈ 마지막 rename 시간 (~1 ms)
```
현재 단계에서는 단순성을 우선하여 stop-the-world 전략을 채택한다.

**POSIX rename 전제**
`os.Rename`의 atomic 보장은 POSIX 파일시스템에서만 유효하다. 같은 filesystem 내 rename이어야 한다 (tmpPath와 walPath가 같은 마운트 포인트에 있어야 한다).

---

## Alternatives Considered

### Background Compaction (copy-then-swap)

lock 없이 temp 파일을 기록하고, 마지막 rename 시점에만 짧게 lock을 잡는 방식.

**거부 이유:**
lock 없이 쓰는 구간 동안 새 write가 원본 WAL에 계속 append된다. temp 파일에는 이 새 write가 없어서 rename 후 index가 존재하지 않는 offset을 가리킬 수 있다. 이를 해결하려면 compaction 시작 이후의 delta write를 추적하고 temp 파일에 병합해야 한다. 구현 복잡도가 stop-the-world 대비 3배 이상이다. Phase 3에서 실제 성능 문제가 관찰되면 도입한다.

### Segment Rotation + Per-Segment Compaction

64MB 단위로 segment를 분리하고 각 segment를 독립적으로 compact하는 방식.

**거부 이유:**
multi-segment index (key → (segment_id, offset)) 관리, segment GC, read-path에서 다중 파일 참조 등 복잡도가 크게 증가한다. 현재 Phase 2에서 불필요하다.

---

## Related Decisions

- **ADR-005: Hash Index KV Store** — Compact()는 HashIndex.Snapshot()으로 live key 목록을 얻는다. 새 index는 NewHashIndex()로 재생성된다.
- **ADR-004: WAL Reader Design** — writeCompactedWAL이 ReadRecordAt()을 사용해 old offset에서 레코드를 읽는다.
- **ADR-001: Pure Go Standard Library** — 외부 의존성 없이 os.Rename, sync.RWMutex, atomic으로 구현.
