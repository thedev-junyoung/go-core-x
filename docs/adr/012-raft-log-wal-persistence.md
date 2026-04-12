# ADR-012: Raft 로그 WAL 영속화

**상태**: Accepted
**날짜**: 2026-04-12
**관련**: [ADR-011](011-raft-log-persistence-and-role-transition.md)

---

## 문제

Phase 5b까지 `RaftNode.log ([]LogEntry)`는 메모리에만 존재했다. 노드가 재시작되면 로그가 사라지고 `lastLogIndex=0`, `lastLogTerm=0`으로 초기화된다.

이 상태는 두 가지 안전 위반을 유발한다.

**§5.4.1 Election Restriction 무력화**: 이전 리더였던 노드가 재시작하면, 실제로는 다른 노드보다 로그가 앞서 있었음에도 `lastLogIndex=0`으로 선거에 참여한다. `candidateLogUpToDate(0, 0)`은 상대 노드도 `lastLogIndex=0`이면 통과하므로 stale 노드가 당선될 수 있다.

**복구 후 log divergence**: 리더 재시작 후 followers와의 `nextIndex` 수렴 과정에서 `prevLogIndex/prevLogTerm` 체크가 실패한다. 리더 로그는 비어 있지만 follower들은 커밋된 항목을 갖고 있어 클러스터가 진전하지 못한다.

---

## 결정

`LogEntry` 슬라이스를 별도의 WAL 파일(`raft_log.wal`)에 영속화하는 `LogStore` 인터페이스를 도입하고, `WALLogStore`가 기존 `wal.Writer`/`wal.Reader` 인프라를 재사용하도록 구현한다.

---

## 기각된 대안들

### 기존 events.wal에 embed

이벤트 WAL에 `LogEntry`를 함께 기록하는 방법이다. 세 가지 이유로 기각했다.

**회복 비용**: 재시작마다 수 GB의 이벤트 WAL을 전체 스캔해서 Raft 로그 엔트리를 추출해야 한다. MetaStore와 같은 이유로 기각 (ADR-011 §1 참조).

**레코드 타입 혼재**: 이벤트 WAL의 리더/팔로워 구분, compaction 로직이 Raft 로그 엔트리를 인식해야 한다. 계층 의존성이 역전된다.

**독립적 수명**: events.wal은 compaction으로 교체될 수 있지만 Raft 로그는 독립적인 수명을 갖는다.

### 새 바이너리 포맷 직접 구현

Raft 로그 전용 포맷을 처음부터 구현하는 방법이다. 이미 magic + CRC32 + 버퍼풀을 갖춘 `wal.Writer`가 있으므로 재발명이다. 기각.

### protobuf 직렬화

`pb.LogEntry`를 그대로 직렬화하는 방법이다. 단순 binary encoding 대비 오버헤드가 있고, 디코딩 시 할당이 발생한다. 기각.

---

## 설계 세부

### LogStore 인터페이스

```go
type LogStore interface {
    Append(entry LogEntry) error
    TruncateSuffix(fromIndex int64) error
    LoadAll() ([]LogEntry, error)
    Close() error
}
```

MetaStore와 동일한 설계 철학: 인터페이스로 추상화하여 테스트에서 `MemLogStore`를 주입한다.

### 페이로드 포맷

```
typeEntry    [0x01] [Index:8 LE] [Term:8 LE] [DataLen:4 LE] [Data:N]
typeTruncate [0x02] [FromIndex:8 LE]
```

WAL 레코드(magic + timestamp + CRC32)가 이 페이로드를 감싸므로 무결성 검사는 WAL 레이어에서 처리된다.

### Truncation — append-only tombstone 방식

Raft §5.3에서 follower는 충돌하는 엔트리를 잘라내야 한다. WAL은 append-only이므로 "파일 재작성"과 "tombstone append" 두 가지 방법이 있다.

tombstone을 선택한 이유:

- **쓰기 경로 단순**: `TruncateSuffix`는 단순히 `typeTruncate` 레코드를 append한다. 파일 재작성은 배타적 락, 임시 파일, atomic rename이 필요하다.
- **충돌 안전**: crash 시 tombstone이 부분적으로 기록되면 WAL의 CRC32가 감지하고 ErrTruncated로 처리한다. 재작성 중 crash는 파일이 손상될 수 있다.
- **빈도**: 로그 충돌은 leader 교체 시에만 발생하고, 정상 운영에서는 드물다. 파일에 tombstone이 누적되는 속도는 무시할 수 있다.

`LoadAll`에서 tombstone을 재생하면 올바른 로그를 재구성한다.

### SyncImmediate 정책

`WALLogStore`는 `SyncImmediate` 정책을 강제한다. Raft §5에서 로그 엔트리는 ACK 전에 반드시 디스크에 durable해야 한다. `SyncInterval`로 배치 fsync를 허용하면, 간격 내 crash 시 leader에게 Success를 응답했지만 실제로 기록되지 않은 엔트리가 생긴다.

**Safety > Throughput**: Raft 로그 append의 빈도는 heartbeat interval(50ms)에 비례하며, 이벤트 수집 경로(events.wal)에 비해 훨씬 낮다. SyncImmediate의 성능 페널티는 여기서 수용 가능하다.

### persist-before-update 순서

```go
// HandleAppendEntries 내 truncation
if n.logStore != nil {
    if err := n.logStore.TruncateSuffix(pbEntry.Index); err != nil {
        return AppendEntriesResult{Success: false}
    }
}
n.log = n.log[:cutAt]  // 영속화 성공 후에만 메모리 수정

// HandleAppendEntries 내 append
if n.logStore != nil {
    if err := n.logStore.Append(...); err != nil {
        return AppendEntriesResult{Success: false}
    }
}
n.log = append(n.log, ...)  // 영속화 성공 후에만 메모리 수정
```

순서가 반대면:
- 메모리 업데이트 → crash → 재시작 → 영속화되지 않은 항목이 메모리에서 사라짐
- leader는 Success를 받았지만 follower 재시작 후 로그 불일치

### 재시작 복구

```go
// NewRaftNode
if logStore != nil {
    entries, err := logStore.LoadAll()
    // 에러 시 빈 로그로 시작 (로그 필요)
    n.log = entries
}
```

`LoadAll`은 WAL 전체를 순차 스캔하며 typeEntry/typeTruncate 레코드를 재생한다. `ErrTruncated`(crash 시 마지막 레코드 partial write)는 무시한다 — 해당 항목은 ACK되지 않았으므로 leader가 재전송한다.

---

## 결과

| 속성 | 이전 | 이후 |
|---|---|---|
| 재시작 후 로그 | 소실 (`lastLogIndex=0`) | WAL에서 복구 |
| §5.4.1 Election Restriction | 재시작 후 무력화 | 재시작 후에도 유효 |
| 로그 충돌 처리 | 메모리 truncate only | tombstone + 메모리 truncate |
| 테스트 | `nil` logStore로 비활성화 가능 | `MemLogStore` 주입으로 격리 테스트 |
| 파일 위치 | — | `data/raft_log.wal` |
