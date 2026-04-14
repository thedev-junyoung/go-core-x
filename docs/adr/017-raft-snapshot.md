# ADR-017: Raft Log Compaction — Snapshot Mechanism

**상태**: Accepted
**날짜**: 2026-04-14
**검토**: 2026-04-14 (core-x-principal-architect 아키텍처 검토 반영)
**관련**: [ADR-016](016-raft-kv-durability.md), [ADR-012](012-raft-log-wal-persistence.md), [ADR-006](0006-wal-compaction.md)

---

## 문제

### WAL 무한 증가 문제

ADR-016에서 확정된 내구성 설계는 두 개의 WAL을 운용한다.

```
data/raft_log.wal   — Raft 로그 엔트리 (SyncImmediate)
data/kv.wal         — KV 상태 머신 결과 (SyncInterval 100ms)
```

`data/raft_log.wal`은 현재 삭제 메커니즘이 없다. `WALLogStore.TruncateSuffix`는 충돌 해소를 위한 suffix 제거만 지원하며, prefix 삭제(오래된 커밋된 엔트리 제거)는 구현되지 않았다.

장기 운용 시나리오:

```
엔트리 크기 평균:  ~100 bytes (RaftKVCommand JSON + 헤더 오버헤드)
초당 쓰기 처리량:  1,000 ops/s (보수적 추정)
하루 생성량:       1,000 × 86,400 × 100B ≈ 8.64 GB/day
WAL 보관 기간:     무제한 (현재 설계)
```

이는 두 가지 운영 장애를 유발한다.

**첫째, 재시작 복구 시간 O(N).** `WALLogStore.LoadAll()`은 전체 WAL을 재생한다. 엔트리 수백만 건이 쌓이면 재시작에 분 단위 시간이 걸린다. DDIA §8: "운영자는 수분간 서비스 불가를 허용하지 않는다."

**둘째, 신규 노드 합류 불가.** 새 노드가 클러스터에 합류할 때 리더는 해당 노드의 `nextIndex`부터 최신 로그까지 전송해야 한다. 로그가 수백만 건이면 `AppendEntries`로 따라잡는 데 현실적으로 불가능한 시간이 걸린다. 리더는 **스냅샷**을 전송해야만 효율적인 합류를 지원할 수 있다.

**Raft 논문 §7 Log Compaction**:
> "The Raft log grows during normal operation, but in a practical system the log cannot grow without bound. As the log grows longer, it occupies more space and takes more time to replay."

ADR-016 §기각된 대안들에서 "Phase 9로 연기"한 스냅샷 메커니즘을 이 ADR에서 설계한다.

---

## 전제 조건

이 ADR의 설계는 ADR-016이 완성됐다고 가정한다.

- `KVStateMachine.lastApplied` — 원자적으로 관리되는 체크포인트
- `KVDurableStore` 인터페이스 — `WriteKV`, `DeleteKV`, `RecoverKV`
- `WALLogStore.TruncateSuffix` — suffix 제거 가능 (prefix 제거는 이 ADR에서 추가)
- `kv.Store.RecoverKV()` — `lastAppliedIndex` 복원

---

## 결정

### 설계 원칙

Raft 논문 §7을 따르되, 교육 목적의 명확성을 위해 단계별로 구현한다.

1. **스냅샷 생성**: `lastApplied`까지의 KV 상태를 파일에 덤프한다.
2. **로그 Truncation**: 스냅샷 완료 후 스냅샷 인덱스까지의 Raft 로그를 삭제한다.
3. **InstallSnapshot RPC**: 새 노드가 합류할 때 리더가 스냅샷을 전송한다.
4. **복구**: 재시작 시 스냅샷에서 상태를 복원하고, 스냅샷 이후 로그만 재적용한다.

---

## 1. 스냅샷 트리거 조건

### 트리거 전략

두 가지 트리거를 병행한다.

**트리거 A: 로그 엔트리 수 threshold**

```go
const DefaultSnapshotThreshold = 10_000 // 엔트리 10,000개마다 스냅샷
```

`commitIndex - snapshotIndex >= SnapshotThreshold` 조건이 충족되면 스냅샷을 시작한다. `snapshotIndex`는 가장 최근 스냅샷이 커버하는 마지막 인덱스다.

**트리거 B: 주기적 체크 (보조)**

```go
const DefaultSnapshotCheckInterval = 5 * time.Minute
```

운용 중 트리거 A가 발화하지 않는 저부하 상황에서 WAL이 의도치 않게 쌓이는 것을 방지한다. 단, 실제 스냅샷은 threshold 조건을 함께 만족할 때만 수행한다.

### 트리거 조건 공식

```
should_snapshot = (lastApplied - snapshotIndex) >= SnapshotThreshold
                  AND role == Leader  // 리더만 자발적으로 스냅샷을 시작한다
                  AND !snapshotInProgress  // 동시 스냅샷 방지
```

> **왜 `commitIndex`가 아닌 `lastApplied`인가**: 스냅샷의 `capturedIndex`는 `lastApplied`(상태 머신이 실제로 적용한 인덱스)를 기준으로 한다. `commitIndex > lastApplied`인 순간은 항상 존재하므로 트리거를 `commitIndex`로 평가하면 `capturedTerm = log[capturedIndex].Term` 조회 시 이미 compacted된 인덱스에 접근하는 경우가 발생한다.

**팔로워 스냅샷 제외 이유**: 팔로워는 리더로부터 `InstallSnapshot` RPC를 받는 수동적 경로를 통해서만 스냅샷 상태를 획득한다. 팔로워가 독립적으로 스냅샷을 생성하면 스냅샷 인덱스/텀 정보가 리더와 달라질 수 있으며, 이는 §7의 안전 조건을 위반한다.

### Config 구조체 확장

```go
// SnapshotConfig는 스냅샷 생성 정책을 정의한다.
type SnapshotConfig struct {
    // Threshold: 새 엔트리가 이 수만큼 쌓이면 스냅샷을 시작한다.
    // 0이면 자동 스냅샷 비활성화.
    Threshold int64

    // CheckInterval: 주기적 트리거 체크 간격.
    CheckInterval time.Duration

    // Dir: 스냅샷 파일 저장 디렉터리.
    Dir string
}
```

---

## 2. 스냅샷 생성 플로우 (Stop-the-world 최소화)

### 핵심 문제: "스냅샷 생성 중 서비스 중단 최소화"

단순 구현은 스냅샷 생성 동안 `KVStateMachine.mu`를 잡고 전체 `data` 맵을 직렬화한다. 이는 `Get`과 `apply`를 차단하는 stop-the-world 방식이다.

Core-X에서 이를 회피하는 방법은 두 가지다.

**Option A: Copy-on-Write (COW) 스냅샷**

`sm.data`의 얕은 복사본을 만들고, 뮤텍스를 즉시 해제한 뒤 복사본을 별도 고루틴에서 직렬화한다.

```
latency impact: O(N) 복사 (N = 키 수)
lock hold time: ~수십 μs (맵 복사만)
메모리 비용: 스냅샷 기간 동안 sm.data 메모리 × 2
```

**Option B: 별도 goroutine + 스냅샷 인덱스 freeze**

`snapshotIndex = lastApplied.Load()` 값을 캡처한 뒤, 해당 인덱스까지의 상태를 Bitcask WAL에서 읽어 파일로 덤프한다. 이 방식은 `sm.data`를 전혀 읽지 않으므로 뮤텍스가 필요 없다.

```
latency impact: Bitcask 순차 읽기 (background goroutine에서)
lock hold time: 0
메모리 비용: 스냅샷 파일 크기만큼의 쓰기 버퍼
```

### 선택: Option A (COW) — Phase 9 구현

**이유:**

1. Core-X의 `sm.data`는 `map[string]string`이다. Go의 맵 복사는 레퍼런스만 복사하는 얕은 복사가 아니라 `make + 루프`가 필요하다. 키-값이 모두 immutable string이므로 값 자체의 복사 비용은 없고, 맵 버킷 구조 복사만 발생한다.
2. Option B는 Bitcask WAL에 `AllKeys()` API가 필요하다. `kv.Store`의 `HashIndex`가 offset만 저장하므로, "모든 키의 현재 값"을 읽으려면 인덱스를 순회하며 WAL을 재읽어야 한다. 이는 랜덤 I/O로 Option A보다 느리다.
3. 뮤텍스 보유 시간이 짧고(복사만), 복사 완료 후 서비스가 즉시 재개된다.

**Trade-off:**

- 최적화: 뮤텍스 보유 시간 최소화 (서비스 중단 < 1ms for 100k keys)
- 희생: 스냅샷 기간 동안 메모리 사용량 2배. 기본 threshold 10,000 엔트리에서 평균 KV 크기 200B로 계산하면 추가 메모리 ≈ 2MB — 허용 가능.
- 한계: 단일 키가 매우 크거나 키가 수백만 건이면 복사 시간이 수십ms로 늘어나 뮤텍스 홀드 시간 허용 범위를 초과할 수 있다. 이 경우 Phase 10에서 MVCC 방식으로 전환한다.

### 스냅샷 생성 시퀀스

```
Step 1: 트리거 조건 확인 (apply loop or ticker)
  if (commitIndex - snapshotIndex) < Threshold: skip

Step 2: snapshotInProgress = true (atomic CAS)
  실패 시: 이미 진행 중 → skip

Step 3: snapshotIndex 캡처
  capturedIndex = lastApplied.Load()
  capturedTerm  = log[capturedIndex].Term  (n.mu 하에서 조회)

Step 4: sm.mu.RLock() 하에서 lastApplied와 sm.data를 동시에 캡처 (COW correctness)
  sm.mu.RLock()
  capturedIndex = sm.lastApplied.Load()  // ← n.mu에서 읽은 값과 동일해야 함
  snapshot := make(map[string]string, len(sm.data))
  for k, v := range sm.data { snapshot[k] = v }
  sm.mu.RUnlock()
  // 이후 sm.data는 자유롭게 변경 가능

  // 주의: capturedIndex와 sm.data를 같은 sm.mu.RLock 컨텍스트에서 캡처해야
  // "인덱스 N까지의 상태를 정확히 담은 스냅샷"이 보장된다.
  // Step 3에서 n.mu로 읽은 capturedIndex와 여기서 읽은 sm.lastApplied가
  // 다를 경우(apply loop가 앞섰을 때)는 스냅샷을 중단하거나 새 값을 사용한다.

Step 5: 별도 고루틴에서 스냅샷 파일 기록
  go func() {
      defer func() { atomic.StoreInt32(&snapshotInProgress, 0) }()
      if err := writeSnapshotFile(capturedIndex, capturedTerm, snapshot); err != nil {
          slog.Error("raft: snapshot write failed", ...)
          return
      }
      // Step 6: 성공 시 Raft 로그 prefix 삭제
      n.truncateLogPrefix(capturedIndex)
  }()
```

**고루틴 분리 이유**: Step 5의 파일 I/O를 apply loop와 분리하여 스냅샷 진행 중에도 새 엔트리가 계속 apply될 수 있도록 한다. `snapshotInProgress` 플래그로 동시 스냅샷을 방지한다.

---

## 3. 스냅샷 파일 포맷

### 파일 구조

```
data/snapshots/snapshot-{index}-{term}.snap
```

파일 포맷 (바이너리):

```
[Header]
  Magic:    4 bytes  (0xRAFT5NAP)
  Version:  2 bytes  (현재: 0x0001)
  Index:    8 bytes  (snapshotLastIndex, big-endian)
  Term:     8 bytes  (snapshotLastTerm, big-endian)
  KVCount:  8 bytes  (key-value 쌍 수)

[Body: KVCount개의 레코드]
  KeyLen:   2 bytes
  Key:      KeyLen bytes
  ValLen:   4 bytes
  Value:    ValLen bytes

[Footer]
  CRC32:    4 bytes  (Header + Body 전체에 대한 체크섬)
```

**설계 선택 이유:**

1. **자체 포맷 (vs JSON/protobuf)**: 순차 바이너리 읽기로 복원 속도 최대화. JSON은 파싱 오버헤드가 크고 숫자 필드에서 정밀도 위험이 있다.
2. **CRC32 footer**: 파일 오염 감지. 읽기 완료 후 CRC32를 검증하여 부분 쓰기와 비트 rot를 구분한다.
3. **별도 파일명에 index/term 인코딩**: 파일명만으로 "어느 스냅샷이 더 최신인가"를 즉시 알 수 있다. 메타데이터 파일 불필요.
4. **atomic 교체**: 파일은 `.tmp` 확장자로 먼저 기록한 뒤 `os.Rename`으로 원자적으로 교체한다. 부분 기록된 스냅샷이 유효한 파일처럼 보이는 것을 방지한다.

```go
// 원자적 스냅샷 파일 기록
tmpPath := fmt.Sprintf("%s/snapshot-%d-%d.tmp", dir, index, term)
finalPath := fmt.Sprintf("%s/snapshot-%d-%d.snap", dir, index, term)

f, _ := os.Create(tmpPath)
writeSnapshotData(f, index, term, kv)
f.Sync()
f.Close()
os.Rename(tmpPath, finalPath) // atomic on POSIX
```

---

## 4. 로그 Truncation 전략 (WAL Prefix 삭제)

### 현재 상태

`WALLogStore`는 `TruncateSuffix` (충돌 해소용)만 지원한다. 스냅샷 완료 후 오래된 prefix를 삭제하는 `TruncatePrefix`가 없다.

### 결정: In-memory 즉시 삭제 + WAL 재작성

스냅샷이 완료되면 두 단계로 로그를 압축한다.

**Step A: In-memory 로그 슬라이스 축소**

```go
// n.mu를 잡고 실행
func (n *RaftNode) truncateLogPrefix(upToIndex int64) {
    n.mu.Lock()
    defer n.mu.Unlock()

    // log는 0-indexed; log[i].Index == i+1
    cutoff := int(upToIndex) // log[:cutoff]를 버린다
    if cutoff > len(n.log) {
        cutoff = len(n.log)
    }
    n.log = n.log[cutoff:] // 슬라이스 재슬라이싱 (O(1), 헤더만 이동)
    n.snapshotIndex = upToIndex
    n.snapshotTerm  = term
}
```

`n.log = n.log[cutoff:]`는 GC pressure를 남긴다 — 앞쪽 엔트리들이 슬라이스 배킹 배열에 여전히 있으나 도달 불가능하다. 이를 해소하려면:

```go
newLog := make([]LogEntry, len(n.log)-cutoff)
copy(newLog, n.log[cutoff:])
n.log = newLog
```

명시적 복사로 GC 참조를 끊는다. 단, 이 복사는 O(N-cutoff)이다. Phase 9에서는 재슬라이싱으로 시작하고, 벤치마크로 GC 영향을 측정한 뒤 결정한다.

**Step B: WAL 파일 재작성 (압축)**

In-memory 압축 후 `WALLogStore`의 WAL 파일을 재작성한다. `wal.Writer.RunExclusiveSwap`이 이미 이 패턴을 지원한다 (ADR-006, `kv.Store.Compact`).

```go
func (s *WALLogStore) CompactPrefix(upToIndex int64) error {
    // 현재 WAL에서 upToIndex 이후 엔트리만 유지한 새 파일 작성
    tmpPath := s.path + ".compact.tmp"
    return s.w.RunExclusiveSwap(func() (*os.File, int64, error) {
        // 1. s.path를 읽어 upToIndex 초과 엔트리만 tmpPath에 쓴다
        // 2. tmpPath를 s.path로 rename
        // 3. 새 파일 핸들 반환
        return rewriteWALExcluding(s.path, tmpPath, upToIndex)
    })
}
```

**재작성 타이밍**: 스냅샷 파일이 fsync + rename으로 영속화된 이후에만 WAL 재작성을 수행한다. 순서:

```
1. 스냅샷 파일 기록 + fsync + rename (atomic)
2. In-memory log 슬라이스 축소
3. WALLogStore.CompactPrefix(snapshotIndex) — WAL 파일 재작성
```

Step 1이 실패하면 Step 2, 3을 수행하지 않는다. Step 3이 실패해도 스냅샷 파일은 이미 있으므로 다음 재시작에서 복구 가능하다.

**Trade-off:**

- 최적화: 재시작 복구 시간 단축 (LoadAll이 재작성된 작은 WAL만 읽음)
- 희생: `RunExclusiveSwap`이 모든 WAL 쓰기를 잠시 차단 (~수ms). Raft 로그 기록 차단은 AppendEntries latency에 직접 영향을 준다.
- 한계: 컴팩션 WAL 재작성 중 프로세스 크래시 → `.compact.tmp`가 남는다. 재시작 시 `.tmp` 파일을 정리하는 스타트업 로직이 필요하다.

---

## 5. InstallSnapshot RPC 설계 (신규 노드 합류)

### 시나리오

노드 C가 클러스터에 합류할 때, 리더 A의 `nextIndex[C] <= snapshotIndex`인 경우 — 즉 C가 필요한 로그가 이미 압축됐을 때 — 리더는 `AppendEntries` 대신 `InstallSnapshot`을 보낸다.

Raft 논문 §7:
> "If the follower's log is too far behind, the leader sends a snapshot."

### RPC 정의

```protobuf
message InstallSnapshotRequest {
    int64  term               = 1;
    string leader_id          = 2;
    int64  last_included_index = 3; // 스냅샷이 커버하는 마지막 로그 인덱스
    int64  last_included_term  = 4; // 해당 인덱스의 텀
    int64  offset             = 5; // 청크 파일 내 byte offset
    bytes  data               = 6; // 스냅샷 청크 (최대 1MB)
    bool   done               = 7; // 마지막 청크면 true
}

message InstallSnapshotResponse {
    int64 term = 1; // 수신자의 currentTerm (리더 step-down 판정용)
}
```

**청크 분할 이유**: 스냅샷 파일이 수십 MB에 달할 수 있다. 단일 RPC로 전송하면 gRPC 메시지 크기 제한(기본 4MB)을 초과하거나, 네트워크 파티션 중에 일부만 전송된 상태에서 재시도가 어렵다. 1MB 청크로 분할하면:
- 재시도 단위가 작아진다 (부분 실패 시 해당 청크만 재전송)
- 수신 측이 청크를 임시 파일에 순서대로 기록하고, `done=true`에서 rename한다

### 리더 측 플로우

```
HandleInstallSnapshot (leader-side send loop):

for each chunk in snapshotFile:
    req = InstallSnapshotRequest{
        term:               currentTerm,
        leader_id:          n.id,
        last_included_index: snapshotIndex,
        last_included_term:  snapshotTerm,
        offset:             chunkOffset,
        data:               chunk,
        done:               isLastChunk,
    }
    resp, err = peer.InstallSnapshot(req)
    if err: retry with backoff
    if resp.term > currentTerm: step down to follower
    if done: update nextIndex[peer] = snapshotIndex + 1
             update matchIndex[peer] = snapshotIndex
```

### 팔로워 측 플로우

```
HandleInstallSnapshot (follower-side):

1. term 검증: req.term < currentTerm → respond with currentTerm, return
2. 리더 인정: n.leaderID = req.leader_id; reset election timer
3. 청크 수신:
   if req.offset == 0: 임시 파일 생성 (snapshot-{index}.tmp)
   임시 파일에 req.data 쓰기 (offset 위치)
4. 마지막 청크 (req.done == true):
   a. CRC32 검증 (전체 파일 체크섬)
   b. 검증 실패 → 임시 파일 삭제, 에러 로그
   c. 검증 성공 → 파일 rename (snapshot-{index}-{term}.snap)
5. 스냅샷 적용:
   a. 기존 sm.data 초기화
   b. 스냅샷 파일에서 KV 복원
   c. sm.lastApplied = req.last_included_index
   d. n.log = []  (스냅샷 이전 로그 전부 버림)
   e. n.snapshotIndex = req.last_included_index
   f. n.snapshotTerm  = req.last_included_term
   g. n.commitIndex   = max(commitIndex, snapshotIndex)
6. respond: {term: currentTerm}
```

**중요 안전 조건 (§7)**: 팔로워가 스냅샷을 적용하기 전에 자신의 로그에 스냅샷 인덱스/텀과 일치하는 엔트리가 있으면, 그 이후 로그를 보존해야 한다 (더 최신 정보가 있을 수 있음). 기본 구현에서는 단순화를 위해 항상 스냅샷을 덮어쓴다.

```go
// §7 최적화 (Phase 9에서 구현)
// 팔로워 로그에 스냅샷 인덱스/텀과 일치하는 엔트리가 있고,
// 해당 인덱스까지 이미 apply된 경우에만 prefix 삭제로 최적화한다.
//
// 안전 조건 3가지를 모두 만족해야 한다:
//   1. 로그에 req.LastIncludedIndex 엔트리가 존재
//   2. 해당 엔트리의 Term이 req.LastIncludedTerm과 일치 (Log Matching Property)
//   3. sm.lastApplied >= req.LastIncludedIndex (해당 인덱스까지 실제 apply됨)
entry, ok := n.entryAt(req.LastIncludedIndex)
if ok && entry.Term == req.LastIncludedTerm &&
    sm.lastApplied.Load() >= req.LastIncludedIndex {
    n.truncateLogPrefix(req.LastIncludedIndex, req.LastIncludedTerm)
    // sm.data는 유지 — 이미 최신 상태
    return
}
// 그렇지 않으면 전체 스냅샷 적용
```

> **조건 2 (Term 일치) 추가 이유**: 인덱스만 비교하면 서로 다른 term의 동일 인덱스를 가진 엔트리를 허용하게 되어 Log Matching Property를 위반한다. 팔로워가 term이 다른 엔트리를 가지고 있다면 전체 스냅샷 적용이 안전하다.
>
> **조건 3 (lastApplied) 추가 이유**: 로그에 엔트리가 있어도 아직 apply되지 않았으면 sm.data가 해당 인덱스까지의 상태를 반영하지 않는다. sm.data를 유지하면 apply되지 않은 변경이 누락된다.

---

## 6. 체크섬/무결성 검증 및 오염 복구 시나리오

### 체크섬 검증 시점

```
생성 시:  파일 기록 완료 → CRC32 계산 → footer에 기록 → fsync → rename
복원 시:  파일 오픈 → 전체 읽기 → CRC32 재계산 → footer와 비교
```

### 오염 시나리오별 복구 전략

**시나리오 A: 스냅샷 파일 CRC32 불일치 (비트 rot, 부분 기록)**

```
재시작 시 detect:
  open(snapshot-42-3.snap) → read all → CRC32 mismatch

복구 경로:
  1. 해당 스냅샷 파일을 무효화 (rename to .corrupt)
  2. 이전 스냅샷 파일 검색 (snapshot-{N<42}-*.snap)
  3. 이전 스냅샷에서 복원 후, Raft 로그에서 gap 재적용
  4. 이전 스냅샷도 없으면: Raft WAL 전체 재생 (index 0부터)
  5. Raft WAL도 손상됐으면: 데이터 손실 불가피 → 운영자 개입 요구
```

복수의 스냅샷 파일을 보관하는 이유가 여기 있다. **최소 2개의 스냅샷**을 유지한다.

```go
const DefaultSnapshotRetainCount = 2

// 새 스냅샷 성공 후 가장 오래된 것을 삭제
func pruneOldSnapshots(dir string, retainCount int) {
    snaps := listSnapshots(dir) // index 기준 내림차순 정렬
    for _, s := range snaps[retainCount:] {
        os.Remove(s.path)
    }
}
```

**시나리오 B: 스냅샷 기록 중 프로세스 크래시 (`.tmp` 파일 잔존)**

```
재시작 시 detect:
  glob("data/snapshots/*.tmp") → 존재

복구 경로:
  1. 모든 .tmp 파일 삭제 (부분 기록 = 무효)
  2. 완전한 .snap 파일에서 복원 진행
```

**시나리오 C: 스냅샷 적용 후 Raft WAL 압축 전 크래시**

```
상태: 스냅샷 파일 존재 (유효), WAL에 구 엔트리 아직 존재

재시작 시:
  스냅샷 로드 → sm.lastApplied = snapshotIndex
  WAL 로드 → snapshotIndex 이하 엔트리는 already-applied로 skip
  (LoadAll에서 snapshotIndex 이하 엔트리를 무시하는 로직 필요)
```

이는 WAL prefix 삭제가 멱등성을 가져야 함을 의미한다. `LoadAll`은 `snapshotIndex` 이하 인덱스의 엔트리를 조용히 무시해야 한다.

**시나리오 D: InstallSnapshot 수신 중 팔로워 크래시 (`.tmp` 청크 파일 잔존)**

시나리오 B와 동일하게 처리: `.tmp` 파일 삭제 후 재시작. 리더는 이후 다시 `InstallSnapshot`을 시도한다.

### 무결성 검증 인터페이스

```go
// SnapshotVerifier는 스냅샷 파일의 무결성을 검증한다.
type SnapshotVerifier interface {
    // Verify는 파일 전체를 읽고 CRC32를 검증한다.
    // 유효하면 nil, 손상이면 ErrCorruptedSnapshot을 반환한다.
    Verify(path string) error
}
```

---

## 7. Go 인터페이스 초안

### Snapshotable

`KVStateMachine`이 구현해야 할 스냅샷 인터페이스:

```go
// Snapshotable은 상태 머신의 스냅샷 생성/복원 능력을 추상화한다.
//
// 구현체: KVStateMachine
// 소비자: RaftNode (스냅샷 트리거), InstallSnapshot RPC 핸들러 (복원)
type Snapshotable interface {
    // TakeSnapshot은 현재 상태의 스냅샷을 반환한다.
    //
    // 반환된 SnapshotData는 호출 시점의 상태를 나타내며,
    // 이후 상태 변경에 영향을 받지 않는다 (COW 보장).
    //
    // lastApplied: 스냅샷이 커버하는 마지막 Raft 인덱스
    // lastTerm은 반환하지 않는다 — term은 Raft 레이어의 메타데이터이므로
    // 호출자(RaftNode)가 entryAt(lastApplied).Term으로 직접 조회한다.
    TakeSnapshot() (data SnapshotData, lastApplied int64, err error)

    // RestoreSnapshot은 스냅샷 데이터로 상태를 완전히 교체한다.
    //
    // 호출 전에 기존 상태가 초기화되어야 한다.
    // InstallSnapshot RPC 적용 시 팔로워에서 호출된다.
    RestoreSnapshot(data SnapshotData) error
}

// SnapshotData는 상태 머신의 직렬화 가능한 스냅샷이다.
// KV 스토어에서는 map[string]string의 스냅샷이다.
type SnapshotData struct {
    KV map[string]string
}
```

### SnapshotStore

스냅샷 파일의 읽기/쓰기/열거를 담당하는 스토리지 인터페이스:

```go
// SnapshotStore는 스냅샷 파일의 영속화를 담당한다.
//
// 구현체: FileSnapshotStore (data/snapshots/ 디렉터리 기반)
// 소비자: RaftNode
type SnapshotStore interface {
    // Save는 스냅샷을 파일에 기록하고 영속화한다 (fsync + rename).
    // 성공 시 SnapshotMeta를 반환한다.
    Save(meta SnapshotMeta, data SnapshotData) error

    // Load는 지정된 인덱스의 스냅샷을 로드한다.
    // ErrSnapshotNotFound: 해당 인덱스의 스냅샷 없음
    // ErrCorruptedSnapshot: CRC32 불일치
    Load(index int64) (SnapshotMeta, SnapshotData, error)

    // Latest는 가장 최신 스냅샷의 메타를 반환한다.
    // 스냅샷이 없으면 (SnapshotMeta{}, ErrSnapshotNotFound)를 반환한다.
    Latest() (SnapshotMeta, error)

    // List는 보관 중인 모든 스냅샷을 최신순으로 반환한다.
    List() ([]SnapshotMeta, error)

    // Prune은 retainCount 이후의 오래된 스냅샷을 삭제한다.
    Prune(retainCount int) error
}

// SnapshotMeta는 스냅샷 파일의 식별 메타데이터다.
type SnapshotMeta struct {
    Index     int64     // 스냅샷이 커버하는 마지막 Raft 인덱스
    Term      int64     // 해당 인덱스의 텀
    CreatedAt time.Time // 생성 시각
    Size      int64     // 파일 크기 (bytes)
    CRC32     uint32    // 전체 파일 체크섬 (header + body)
}
```

### LogStore 확장

```go
// LogStore 인터페이스에 CompactPrefix 추가 (ADR-012 원본 + 이 ADR 확장)
type LogStore interface {
    Append(entry LogEntry) error
    TruncateSuffix(fromIndex int64) error
    LoadAll() ([]LogEntry, error)

    // CompactPrefix removes all entries with Index <= upToIndex from stable storage.
    //
    // Must be called AFTER the corresponding snapshot has been durably saved.
    // On success, subsequent LoadAll calls will not return compacted entries.
    //
    // Safety: if the node crashes between snapshot save and CompactPrefix,
    // LoadAll returns entries that were already covered by the snapshot.
    // The caller (RaftNode.recover) must skip entries with index <= snapshotIndex.
    CompactPrefix(upToIndex int64) error

    Close() error
}
```

### lastEntry() 수정 — snapshotIndex Fallback 필수

스냅샷 도입 후 `n.log`는 `snapshotIndex + 1`부터 시작하는 슬라이스가 된다. 로그가 비어 있을 때(`len(n.log) == 0`) `lastEntry()`가 `(0, 0)`을 반환하면 §5.4.1 `candidateLogUpToDate` 판정이 깨진다 — 이 노드는 index 0, term 0을 가진 것으로 취급되어 모든 candidate가 더 up-to-date하다고 판정된다.

```go
// lastEntry returns the index and term of the last log entry.
// After compaction, falls back to snapshotIndex/snapshotTerm (§5.4.1).
// Must be called with mu held.
func (n *RaftNode) lastEntry() (index, term int64) {
    if len(n.log) == 0 {
        return n.snapshotIndex, n.snapshotTerm
    }
    e := n.log[len(n.log)-1]
    return e.Index, e.Term
}
```

이 변경은 `candidateLogUpToDate`, `sendHeartbeats`의 `prevLogIndex/prevLogTerm`, `maybeAdvanceCommitIndex` 계산에 전파 효과가 있다 — 모두 `lastEntry()`를 호출하므로 자동으로 올바른 fallback을 사용하게 된다.

### RaftNode 확장

```go
// RaftNode 내 스냅샷 관련 필드 (node.go 확장)
type RaftNode struct {
    // ... existing fields ...

    // snapshotIndex/Term: 가장 최근 스냅샷이 커버하는 마지막 인덱스/텀.
    // §7: "last log index included in the snapshot"
    // 0이면 스냅샷 없음.
    snapshotIndex int64
    snapshotTerm  int64

    // snapshotStore: 스냅샷 파일 저장소. nil이면 스냅샷 비활성화.
    snapshotStore SnapshotStore

    // snapshotCfg: 스냅샷 트리거 정책.
    snapshotCfg SnapshotConfig

    // snapshotInProgress: 동시 스냅샷 방지를 위한 atomic 플래그.
    // 0 = idle, 1 = in-progress
    snapshotInProgress atomic.Int32
}
```

---

## 8. 재시작 복구 절차 (스냅샷 포함)

스냅샷 도입 후 재시작 복구 순서:

```
Step 1: 스냅샷 스토어 초기화
  snapshotStore.Latest() → 최신 스냅샷 메타 확인
  if exists:
    snapshotStore.Load(meta.Index) → CRC32 검증
    if CRC32 OK: sm.RestoreSnapshot(data, meta.Index)  → sm.data + sm.lastApplied 복원
                 n.snapshotIndex = meta.Index  // ← WAL 필터링의 전제 조건
                 n.snapshotTerm  = meta.Term
    if CRC32 fail: 이전 스냅샷 시도 (최대 retainCount만큼)

Step 2: Raft WAL 로드 (ADR-012)
  logStore.LoadAll() → entries
  // snapshotIndex 이하 엔트리 필터링 (slice가 아닌 filter — panic 방지)
  // WAL compaction 완료 시: entries에 구 엔트리가 없음
  // 크래시 시나리오 C: entries에 구 엔트리가 아직 있을 수 있음
  // 양쪽 모두 안전하게 처리:
  filtered := entries[:0]
  for _, e := range entries {
      if e.Index > n.snapshotIndex {
          filtered = append(filtered, e)
      }
  }
  entries = filtered

Step 3: KV WAL 복원 (ADR-016)
  sm.store.RecoverKV() → bitcaskLastApplied
  갭 감지: bitcaskLastApplied vs sm.lastApplied
  갭 재적용: ApplyDirect(entries[bitcaskLastApplied+1..])

Step 4: 일관성 검증
  assert sm.lastApplied == bitcaskLastApplied  // fail-fast
```

---

## 9. 구현 단계 (Phase 분리)

### Phase 9a: 스냅샷 생성 + 로컬 복구

**목표**: 리더가 스냅샷을 자발적으로 생성하고, 재시작 시 스냅샷에서 복원할 수 있다.

구현 범위:
- `SnapshotMeta`, `SnapshotData` 타입 정의
- `Snapshotable` 인터페이스 + `KVStateMachine.TakeSnapshot` / `RestoreSnapshot`
- `FileSnapshotStore` 구현 (Save, Load, Latest, List, Prune)
- `RaftNode`: `snapshotIndex`, `snapshotTerm` 필드 추가
- `RaftNode.maybeSnapshot()` — 트리거 확인 + COW 스냅샷 + goroutine dispatch
- `LogStore.CompactPrefix` 구현 (`WALLogStore.CompactPrefix`)
- `RaftNode.recover()` 확장: 스냅샷 먼저 로드, 이후 WAL gap 재적용
- 스타트업: `.tmp` 파일 정리

테스트:
- 스냅샷 생성 → 프로세스 kill → 재시작 → 데이터 일치 검증
- 스냅샷 파일 CRC32 손상 → 폴백 복구 검증
- 스냅샷 생성 중 동시 쓰기 → 레이스 컨디션 없음 (race detector)

### Phase 9b: InstallSnapshot RPC + 신규 노드 합류

**목표**: 새 노드가 클러스터에 합류할 때 리더로부터 스냅샷을 받아 빠르게 동기화한다.

구현 범위:
- protobuf: `InstallSnapshotRequest`, `InstallSnapshotResponse` 메시지 정의
- gRPC service: `InstallSnapshot` RPC 핸들러 (서버/클라이언트 양측)
- 리더 측: `replicationLoop`에서 `nextIndex[peer] <= snapshotIndex` 감지 → `sendSnapshot(peer)`
- 팔로워 측: `HandleInstallSnapshot` — 청크 수신, CRC32 검증, 상태 교체
- `PeerClients.InstallSnapshot` 메서드 추가

테스트:
- 3노드 클러스터: 노드 하나를 오래 분리 → 스냅샷 생성 → 재합류 → `InstallSnapshot` 경로 검증
- 청크 전송 중 네트워크 오류 → 재시도 검증
- 오염된 청크 수신 → 팔로워가 거절하고 재요청 검증

### Phase 9c: 성능 검증 및 모니터링

모니터링 지표:

```
snapshot_created_total       — 생성된 스냅샷 수
snapshot_size_bytes          — 스냅샷 파일 크기 (히스토그램)
snapshot_duration_seconds    — 생성에 걸린 시간 (COW 복사 + 파일 쓰기)
snapshot_install_total       — InstallSnapshot 수신 횟수
log_compacted_entries_total  — 압축된 로그 엔트리 수
wal_size_bytes               — 현재 WAL 파일 크기 (압축 효과 추적)
```

검증 시나리오:
- 10,000 엔트리 → 스냅샷 → WAL 재시작 복구 시간 < 1s
- COW 스냅샷 중 `Get` 레이턴시 p99 < 5ms (뮤텍스 보유 시간 측정)
- InstallSnapshot: 1MB 청크 × N번 전송 시 팔로워 합류 시간 측정

---

## 결과 (DDIA 3원칙 기준)

### Reliability (신뢰성)

- **개선**: 스냅샷 + 로그 압축으로 재시작 복구 시간이 O(N) → O(스냅샷_이후_엔트리)로 단축된다.
- **개선**: 스냅샷 파일 CRC32 + 다중 보관(retainCount=2)으로 파일 오염에서 자동 폴백이 가능하다.
- **새 위험**: `InstallSnapshot` 도입으로 팔로워가 스냅샷 적용 중 크래시하면 `.tmp` 파일이 잔존한다. 스타트업 정리 로직 필수.
- **새 위험**: COW 스냅샷 중 메모리 2배 사용. OOM kill이 발생하면 스냅샷 실패 + 서비스 재시작이 필요하다. 이는 threshold를 너무 낮게 설정할 때 발생하는 위험이다.

### Scalability (확장성)

- **개선**: 신규 노드가 수백만 건의 로그를 replay하지 않고 스냅샷 하나로 빠르게 합류한다.
- **개선**: WAL 파일 크기가 threshold에 의해 제한된다. 무한 증가 문제 해소.
- **영향**: `CompactPrefix` WAL 재작성이 Raft 로그 쓰기를 수ms 차단한다. 재작성 주기(스냅샷 threshold)를 크게 설정하면 차단 빈도가 줄어든다.

### Maintainability (유지보수성)

- **개선**: `SnapshotStore.List()`로 보관 중인 스냅샷을 즉시 확인할 수 있다. 운영 가시성 향상.
- **개선**: 파일명에 index와 term이 인코딩되어 있어 운영자가 수동으로 상태를 파악할 수 있다.
- **복잡도 증가**: `RaftNode`, `KVStateMachine`, `LogStore`에 스냅샷 관련 상태와 분기가 추가된다. `RaftNode.recover()`의 복구 시퀀스가 더 복잡해진다.
- **테스트 부담**: Phase 9b의 `InstallSnapshot` 경로는 멀티노드 통합 테스트 없이는 검증이 어렵다.

---

## 기각된 대안들

### etcd/raft의 Snapshot 인터페이스를 그대로 차용

etcd/raft는 `raftpb.Snapshot` 메시지와 `Storage.Snapshot()` 인터페이스를 정의한다.

**기각 이유**: Core-X는 교육 목적 프로젝트로, 외부 라이브러리 의존 없이 직접 구현이 목표다 (ADR-001). 또한 etcd의 인터페이스는 우리의 Bitcask WAL 기반 아키텍처와 맞지 않는 추상화를 강제한다.

### RocksDB/LevelDB의 Checkpoint API (SST 파일 하드링크)

파일시스템 하드링크를 이용해 현재 DB 파일을 snapshot 디렉터리에 복사하는 방식. Copy-on-write 없이 O(1)에 스냅샷 생성.

**기각 이유**: Core-X의 Bitcask 구현은 단일 WAL 파일 + 인메모리 인덱스다. RocksDB처럼 다수의 SST 파일이 없으므로 하드링크 기반 체크포인트가 직접 적용되지 않는다. WAL 파일 전체를 하드링크하면 "이 시점의 상태" 격리가 되지 않는다 (이후 append가 같은 파일에 계속 쓰임).

### 외부 스냅샷 스케줄러 (cron / 별도 데몬)

별도 프로세스가 주기적으로 스냅샷을 생성하고 Raft 노드에 신호를 보내는 방식.

**기각 이유**: Raft 스냅샷은 `lastApplied`, `commitIndex`, 로그 상태와 원자적으로 조율돼야 한다. 외부 프로세스는 이 내부 상태에 접근할 수 없어 안전성을 보장하기 어렵다. Raft §7은 상태 머신과 같은 프로세스에서 스냅샷을 생성하도록 설계됐다.

---

## Related Decisions

- [ADR-006](0006-wal-compaction.md): WAL 컴팩션 — `RunExclusiveSwap` 패턴 (이 ADR의 `CompactPrefix`가 재사용)
- [ADR-012](012-raft-log-wal-persistence.md): WAL LogStore — `TruncateSuffix` 구현 (이 ADR에서 `CompactPrefix` 추가)
- [ADR-016](016-raft-kv-durability.md): Bitcask 영속화 — `lastApplied` 체크포인트 (스냅샷의 전제 조건)
