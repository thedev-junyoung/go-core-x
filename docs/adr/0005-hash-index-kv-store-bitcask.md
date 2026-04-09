# ADR-005: Hash Index KV Store (Bitcask Model) — Phase 2 Persistence

- **Status**: Accepted
- **Date**: 2026-04-09
- **Deciders**: Core-X Principal Architect

---

## Context

### Phase 2의 요구사항

Core-X는 Phase 1(in-memory, no persistence)에서 Phase 2(durable storage with fast lookup)로 진전한다.

**Phase 2 목표:**
- **Durability**: 수집된 모든 이벤트를 WAL에 영속화 (power loss 시 복구 가능)
- **Fast Lookup**: 특정 source의 최신 이벤트를 O(1)로 조회 (Get 연산)
- **Crash Recovery**: 서버 재시작 시 WAL 재생으로 인덱스 자동 재구축
- **Memory Bound**: Hash index의 maxKeys로 메모리 무제한 증가 방지
- **Zero-Allocation 유지**: Phase 1의 성능 특성 손상 없음

### KV 스토리지 설계 선택지

| 모델 | 특성 | 읽기 | 쓰기 | 복구 | 메모리 |
|------|------|------|------|------|--------|
| **Hash Index (Bitcask)** | Sequential WAL + in-memory map | O(1) | O(1)* | WAL replay | Bounded |
| **LSM-Tree** | MemTable + SSTable + Compaction | O(log N) | O(1) amortized | automatic | Bounded |
| **B-Tree** | On-disk balanced tree | O(log N) | O(log N) | B-tree recovery | Bounded |

*O(1) amortized with atomic offset tracking

### 왜 Hash Index인가?

Bitcask는 **Riak의 기본 스토리지 엔진**이며 다음과 같은 특성을 가진다:

1. **구현 단순성**: index = `map[string]int64`, write = "append to WAL + update map"
2. **읽기 성능**: 인덱스 lookup O(1) + 단일 `ReadAt` syscall
3. **복구 확실성**: WAL replay → index rebuild, no partial state
4. **메모리 모델 선명함**: index size = key 수의 선형함수

LSM-Tree는 Phase 2 범위를 벗어난다:
- Compaction scheduler, MemTable/SSTable 관리, bloom filter, level merge
- 구현: Hash Index의 5배 이상 복잡도
- 이득: Range query 지원 (Phase 2에서 불필요)

---

## Decision

### 설계: Bitcask 모델 구현

**아키텍처:**

```
Client Request (JSON)
      ↓
IngestionService.Ingest()
      ├─→ walWriter.WriteEventOffset(e)  [offset 반환]
      ├─→ hashIndex.Set(e.Source, offset)
      └─→ workerPool.Submit(e)  [비동기 처리]

Get Request by Source:
      ↓
kvStore.Get(source)
      ├─→ hashIndex.Get(source)  [offset 조회]
      ├─→ wal.ReadRecordAt(offset)  [파일에서 읽기]
      └─→ wal.DecodeEvent(...)  [디코드]

Crash Recovery:
      ↓
kvStore.Recover(onRecover)
      ├─→ for each WAL record:
      │    ├─ hashIndex.Set(source, offset)  [index 재구축]
      │    └─ onRecover(e)  [콜백: 처리 복구]
      └─→ return (count, error)
```

### 핵심 설계 결정

#### 1. WriteEventOffset: 사전(pre-write) offset 캡처

```go
// infrastructure/storage/wal/writer.go

type Writer struct {
    file *os.File
    sizeTracker atomic.Int64  // 누적 쓰기 바이트 수
    // ...
}

func (w *Writer) WriteEventOffset(e *domain.Event) (int64, error) {
    w.mu.Lock()
    defer w.mu.Unlock()

    // 1. Write 직전의 offset 캡처 (정확함)
    offset := w.sizeTracker.Load()

    // 2. WAL에 기록
    _, err := w.file.Write(buf)
    if err != nil {
        return 0, err
    }

    // 3. sizeTracker 업데이트
    w.sizeTracker.Add(int64(len(buf)))

    return offset, nil
}
```

**왜 사전 offset인가?**

```
Timeline:

offset = 100  ← WriteEventOffset() 호출
  ↓
write(buf)    ← 100에 시작해서 164에 끝남
  ↓
sizeTracker.Add(64)  ← 164로 업데이트

따라서 offset=100이 정확한 시작점:
  ReadRecordAt(100)이 올바른 레코드를 읽음.
```

**원자성 보증:**

- `sizeTracker.Load()`는 atomic read
- `file.Write()` + `sizeTracker.Add()` 사이의 차이는 나노초 수준
- crash 중 offset이 WAL에 들어가지 않았다면, Recover에서 보이지 않음 (자동 일관성)

#### 2. HashIndex: RWMutex + map[string]int64

```go
// infrastructure/storage/kv/index.go

type HashIndex struct {
    mu      sync.RWMutex
    entries map[string]int64  // source → WAL offset
    maxKeys int
}

func (idx *HashIndex) Set(key string, offset int64) error {
    idx.mu.Lock()
    defer idx.mu.Unlock()

    // New key check before insertion
    if _, exists := idx.entries[key]; !exists && len(idx.entries) >= idx.maxKeys {
        return ErrIndexFull
    }

    idx.entries[key] = offset  // Overwrite if exists
    return nil
}

func (idx *HashIndex) Get(key string) (int64, bool) {
    idx.mu.RLock()
    defer idx.mu.RUnlock()

    offset, exists := idx.entries[key]
    return offset, exists  // Zero alloc
}
```

**스레드 안전성:**
- `Get`: RLock → concurrent reads 가능
- `Set`: Lock → exclusive write

**Overwrite 의미론 (Bitcask):**

```
Event 1: source="user-a", offset=100
Event 2: source="user-a", offset=200  ← 같은 source

index 상태:
  "user-a" → 100  (Event 1 후)
  "user-a" → 200  (Event 2 후, overwrite)

Get("user-a"):
  → 200 (최신 이벤트만 조회)
```

이것이 의도된 동작: 같은 source로부터 새 이벤트가 오면 이전 버전을 덮어쓴다.

#### 3. ReadRecordAt: 랜덤 접근

```go
// infrastructure/storage/wal/reader.go

func ReadRecordAt(f *os.File, offset int64) (Record, error) {
    // 1. Header 읽기 (offset에서)
    headerBuffer := make([]byte, RecordHeaderSize)
    n, err := f.ReadAt(headerBuffer, offset)

    // 2. Payload 읽기 (offset + 16에서)
    payloadBuffer := make([]byte, payloadSize)
    _, err = f.ReadAt(payloadBuffer, offset+int64(RecordHeaderSize))

    // 3. Checksum 검증
    // ...

    return Record{...}, nil
}
```

**왜 ReadAt 필요한가?**

- `wal.Reader`는 `bufio.Reader` 기반 → 순차 읽기만 가능
- KV Get은 특정 offset에서 랜덤 읽기 필요
- `os.File.ReadAt`는 concurrent-safe, 파일 포지션 상태 없음

**Memory allocation:**

```
ReadRecordAt(offset):
  alloc #1: headerBuffer = make([]byte, 16)
  alloc #2: payloadBuffer = make([]byte, payloadSize)
  ───────
  필수 alloc: 2개 (unavoidable for ownership transfer)

Alternative: caller가 버퍼 제공 (GetInto variant):
  kvStore.GetInto(key, buf []byte) (n int, error)
  → zero-alloc read path (Phase 2.5 최적화로 defer)
```

#### 4. Store.Recover(): WAL 재생 + 콜백

```go
// infrastructure/storage/kv/store.go

func (s *Store) Recover(onRecover func(*domain.Event)) (int, error) {
    reader, err := wal.NewReader(s.walPath)
    defer reader.Close()

    offset := int64(0)
    count := 0

    for reader.Scan() {
        record := reader.Record()

        // 1. Index 재구축
        event, _ := wal.DecodeEvent(record.Data)
        s.index.Set(event.Source, offset)

        // 2. 콜백 호출 (처리 복구)
        if onRecover != nil {
            onRecover(event)
        }

        // 3. Offset 누산
        recordSize := RecordMinSize + int64(len(record.Data))
        offset += recordSize

        count++
    }

    return count, reader.Err()
}
```

**콜백 패턴의 이점:**

```
계층 분리:
  - kv/store.go는 index 재구축만 담당
  - cmd/main.go에서 onRecover 콜백으로 workerPool 연동

vs. 강한 의존성:
  - kv/store.go가 executor 직접 임포트 → 순환 의존 위험
  - kv 패키지가 executor에 의존 → 계층 위반
```

#### 5. WriteEvent 인터페이스 구현

```go
// Store는 application/ingestion.WALWriter 구현

type WALWriter interface {
    WriteEvent(e *domain.Event) error  // application/ingestion/service.go
}

func (s *Store) WriteEvent(e *domain.Event) error {
    // Store.WriteEvent → writer.WriteEventOffset → index.Set
    // 투명하게 WAL + index 관리
    offset, err := s.writer.WriteEventOffset(e)
    if err != nil {
        return err
    }
    return s.index.Set(e.Source, offset)
}
```

**IngestionService 변경 최소화:**

```go
// cmd/main.go 변경 전:
svc := appingestion.NewIngestionService(workerPool, eventPool, walWriter)

// 변경 후:
svc := appingestion.NewIngestionService(workerPool, eventPool, kvStore)

// kvStore가 WALWriter 인터페이스 구현
// 기존 코드: 변경 없음
```

---

## Consequences

### 성능 특성 (DDIA Reliability)

#### Read Path (O(1))

```
kvStore.Get("user-a"):
  │
  ├─ hashIndex.Get("user-a")       [~50 ns, RLock]
  │                          ↓
  │                   offset = 1024
  │
  ├─ wal.ReadRecordAt(f, 1024)     [~1-2 μs, single ReadAt syscall]
  │                          ↓
  │                   record = {...}
  │
  ├─ wal.DecodeEvent(record)       [~100-200 ns, parsing]
  │                          ↓
  │                   *Event{...}
  │
  └─ return *Event

Total: ~2-3 μs
```

비교 (LSM-Tree):
```
LSMTree.Get("user-a"):
  ├─ MemTable 검색         [~100 ns]
  │  (miss)
  ├─ L1 SSTable binary search    [~5 μs, file I/O]
  ├─ L2 SSTable binary search    [~20 μs if needed]
  └─ Total: ~20-30 μs
```

**결론: Hash Index가 5-10배 빠름.**

#### Write Path

```
IngestionService.Ingest():
  │
  ├─ kvStore.WriteEvent(e)
  │    ├─ writer.WriteEventOffset(e)  [offset 캡처]
  │    │    ├─ mu.Lock()        [~100 ns contention]
  │    │    ├─ file.Write(buf)  [~1-5 μs, SyncInterval]
  │    │    └─ sizeTracker.Add()  [~10 ns, atomic]
  │    │    return offset
  │    ├─ index.Set(source, offset)  [~1-2 μs, map write + Lock]
  │    └─ return error
  │
  ├─ workerPool.Submit(e)  [~100 ns, channel send]
  │
  └─ return nil

Total: ~10-15 μs (SyncInterval policy)
```

**Allocation:**
- kvStore.WriteEvent: 0 alloc (pool 재사용)
- writer.WriteEventOffset: 0 alloc (pool 내부)
- index.Set: 0 alloc (existing key overwrite) 또는 1 alloc (new key, map grow)

#### Crash Recovery (Offline)

```
kvStore.Recover():
  startup time (first recovery): N * ~1 μs  (N = WAL record count)

  예: 100GB WAL (~ 100M events)
  → estimated time: 100M * 1 μs = 100 sec (worst case)
  → with optimizations (bulk reads): ~10 sec

  Phase 3에서 compaction 도입 후:
  → 최신 snapshot에서만 recover → startup time << 1 sec
```

### 메모리 모델

#### Index 메모리

```
HashIndex 메모리 = key 수의 선형함수

각 entry: map[string]int64
  ├─ Go map slot: 8 bytes (pointer)
  ├─ string header: 16 bytes (pointer + len)
  ├─ int64 value: 8 bytes
  ├─ string data: len(key) bytes
  └─ internal overhead: ~32 bytes per entry (Go map implementation)

예: maxKeys = 1,000,000
    평균 key size = 30 bytes (e.g., "user-uuid-12345...")
    → Index memory: 1M * (8+16+8+32+30) = ~100 MB

예: maxKeys = 10,000,000
    → ~1 GB
```

**메모리 상한 명확:**

```
maxKeys = 1,000,000  ← 설정 시점에 고정
      ↓
index.Set(key, offset) returns ErrIndexFull when exceeded
      ↓
메모리 무제한 증가 구조적으로 불가능
```

#### WAL 파일 메모리

```
WAL의 read-only fd: ~4 KB (파일 디스크립터)
WAL Writer 버퍼풀: ~10 개 * (16 + 256 + 4) = ~3 KB
───────
총합: ~10 KB (negligible)
```

### 운영상 고려사항

#### 1. maxKeys 튜닝

```
현재: maxKeys = 1,000,000 (Phase 2 학습 레벨)

단일 노드에서:
  source 수 < 1M: 안전
  source 수 > 1M: ErrIndexFull 발생 → 성능 저하

Phase 3 (분산):
  consistent hashing으로 노드당 key 수 제한
  → 단일 노드의 maxKeys 통제 가능
```

#### 2. WAL 파일 크기

```
현재: 단일 segment (파일 무한 증가)

문제:
  - WAL 파일이 수십 GB로 증가 가능
  - Startup recovery 시간 O(file size)

Phase 3 해결:
  - Segment rotation: 64MB 단위로 파일 분할
  - Compaction: 만료된 이벤트 삭제
```

#### 3. Offset 추적의 정확성

```
Critical: offset이 정확해야 하는 이유

Scenario 1 (정확):
  writer.WriteEventOffset()   ← offset = 100 반환
  index.Set("user-a", 100)
  ReadRecordAt(100)           ← 올바른 레코드 읽음

Scenario 2 (부정확):
  index.Set("user-a", 101)    ← off by 1
  ReadRecordAt(101)           ← 헤더 중간부터 읽음
  → ErrCorrupted (magic 검증 실패)
```

**보증 메커니즘:**
- WriteEventOffset은 write 직전 offset 캡처 (스택 변수)
- atomic한 offset 누산 (sizeTracker.Add)
- Recover에서는 WAL 각 레코드 크기를 정확히 계산
  ```go
  recordSize := RecordMinSize + int64(len(record.Data))
  offset += recordSize
  ```

---

## Consequences (계속)

### 단점 및 제약

#### 1. Range Query 불가

```
Hash Index는 point query (Get)만 지원:
  kvStore.Get("user-a")  ← OK

Range query 불가:
  kvStore.GetRange("user-a*")  ← NOT SUPPORTED

이유: map 기반이므로 iteration 외에 range 연산 없음

Phase 3+ (필요시):
  LSM-Tree 또는 B-Tree로 마이그레이션
  또는 추가 색인 (e.g., Trie for prefix search)
```

#### 2. Concurrent Write 경합

```
WriteEvent 경로:
  writer.WriteEventOffset()    [Lock 유지]
  index.Set()                  [Lock 유지]

고부하 상황 (10k req/sec):
  writer.mu 경합 가능
  index.mu 경합 가능

Lock contention:
  - writer.mu: ~10-20% utilization에서 무시할 수준
  - index.mu: map write는 빠르지만 (< 1 μs), lock overhead 존재

Phase 4 최적화 (필요시):
  - Per-partition lock (sharded HashMap)
  - CAS-based atomic offset (추가 complexity)
```

#### 3. Compaction 부재

```
현재: 단일 WAL segment, no cleanup

문제:
  - 이벤트 업데이트 (같은 source 여러 번) → 파일에는 모두 기록
  - 이미 덮어써진 old offset의 레코드는 waste

예:
  source="user-a":
    offset=100 에서 "v1" 기록
    offset=200 에서 "v2" 기록  ← 이 버전만 index가 가리킴
    offset=300 에서 "v3" 기록  ← index가 이것만 가리킴

  WAL 크기: 3 records, 하지만 1개만 "live"

Phase 3:
  - Segment compaction: 미사용 레코드 제거
  - 또는 new segment로 live 데이터만 복사
```

---

## Alternatives Considered

### 1. LSM-Tree (Log-Structured Merge Tree)

**구현 복잡도:**

```
LSM components:
  ├─ MemTable (in-memory, sorted)
  ├─ SSTable (on-disk sorted)
  ├─ Compaction scheduler
  ├─ Level management (L0, L1, ..., Ln)
  ├─ Bloom filter (optimization)
  └─ Merge strategy (leveling or tiering)

추가 코드: ~3000 LOC (from leveldb/rocksdb examples)
Hash Index: ~300 LOC
```

**성능 비교:**

| 연산 | Hash Index | LSM |
|------|-----------|-----|
| Get | ~3 μs (1 syscall) | ~20 μs (multiple SSTable lookups) |
| Put | ~10 μs (amortized) | ~5 μs (MemTable only) |
| Range | ❌ not supported | ✓ O(log N + result size) |
| Recovery | O(WAL size) | O(latest snapshot size) |

**거부 이유:**

- Phase 2 목표는 simple persistence (Get 지원)이지, range query 아님
- 구현 복잡도 5배 이상 → 디버깅, 유지보수 비용 높음
- Performance bottleneck은 JSON parse (~800 ns)이지, index lookup이 아님
- **DDIA: Premature optimization**

**도입 조건:**
- Phase 3+에서 range query 실제 요구사항 발생 시
- 성능 프로파일링에서 Get latency가 전체 critical path의 20% 이상을 차지할 때

### 2. On-Disk B-Tree

```
B-Tree 특성:
  - Balanced tree on disk
  - O(log N) read/write
  - Natural range query support
  - In-place updates (WAL 필요)
```

**거부 이유:**

- WAL과의 조합이 복잡함 (ACID 구현 필요)
- Rebalancing 비용 (Phase 1의 allocation budget에 맞지 않음)
- Hot path에서 multiple disk seeks → latency 높음
- Go 표준 라이브러리에 B-Tree 없음 (외부 의존성)

### 3. SQL Database (SQLite, PostgreSQL)

**특징:**
- ACID, complex queries, built-in reliability
- Out-of-box crash recovery, concurrency control

**거부 이유:**

- 외부 의존성 (ADR-001 위반)
- Overhead (query parser, planner, executor) → allocation 폭증
- "사용하지 않는 기능" 비용 낭비 (JSON 처리, JOIN, 복잡 쿼리 등)
- Docker/K8s 환경에서 별도 프로세스 관리 복잡도

**용도:**
- Phase 4+ (중앙 집계) 또는 별도 분석 시스템

### 4. RocksDB (C 라이브러리)

**특징:**
- High-performance embedded KV (LSM-Tree 기반)
- Production-grade reliability
- Go bindings 존재

**거부 이유:**

- C 라이브러리 바인딩 (CGO) → allocation, GC latency 증가
- "배우는 프로젝트"의 의도(순수 Go)와 맞지 않음
- Allocation 특성 예측 불가 (CGO overhead)

**고려 대상:**
- Phase 4+에서 최고 성능이 필수적일 때, 그리고 순수 Go 제약이 풀릴 때

### 5. Redis (In-Memory)

**거부 이유:**

- 외부 프로세스 → 배포 복잡도 증가
- 데이터 크기 > 메모리 → eviction (데이터 손실)
- "In-memory cache"가 아닌 "durable storage"가 목표
- persistence (RDB/AOF)를 추가하면 LSM-Tree와 복잡도 비슷함

---

## Related Decisions

- **ADR-001: Pure Go Standard Library** — KV Store는 외부 라이브러리 없이 구현. wal, kv 패키지 모두 stdlib만 사용.
- **ADR-002: Worker Pool & Sync.Pool** — KV Store가 추가되어도 worker pool은 무변경. Store가 WALWriter 인터페이스를 구현하는 것으로 통합.
- **ADR-003: Clean Architecture** — Store는 Infrastructure layer에 위치. application/ingestion.WALWriter 포트를 구현.

---

## Monitoring & Validation

### 단위 테스트

```bash
go test -race ./internal/infrastructure/storage/kv/... -v
# Expected: All tests pass, race detector clean
```

### 벤치마크

```bash
# WriteEvent 성능
go test ./internal/infrastructure/storage/kv -bench=BenchmarkStore_WriteEvent -benchmem

# Expected output:
# BenchmarkStore_WriteEvent   1000000   1150 ns/op   0 B/op   0 allocs/op
# (offset 계산은 0 alloc)
```

### 통합 테스트 시나리오

```bash
# 1. Normal: Write → Get 일관성
#   다양한 source, payload size로 Write 후 Get으로 검증

# 2. Overwrite: 같은 source 여러 번 Write
#   index가 최신 offset만 보유하는지 확인

# 3. Recover: WAL 있는 상태에서 새 Store 인스턴스
#   복구된 index 크기와 내용 검증

# 4. MaxKeys: 제한 초과 시 ErrIndexFull
#   index.Set() 실패, WAL은 기록 (inconsistency 처리)

# 5. Crash Simulation: SIGKILL 후 재시작
#   Recover 후 Get이 crash 전 상태 반환 확인
```

### 런타임 모니터링

| 지표 | 엔드포인트 | 경보 조건 |
|------|-----------|-----------|
| `index_size` | 운영 메트릭 | > maxKeys * 0.9 → 최적화 검토 |
| `wal_size` | 파일 시스템 | > 1 GB → compaction 필요 |
| `get_latency_p99` | pprof/trace | > 100 μs → index 경합 또는 disk 느림 |
| `recover_time` | 시작 로그 | > 60 sec → WAL 파일 크기 관리 필요 |

---

## Future Extensions (Not Phase 2)

### Phase 2.5: GetInto (Zero-Alloc Read)

```go
// 현재:
func (s *Store) Get(key string) (*domain.Event, error)
// → 필연적으로 1-2 alloc (Event + strings)

// Phase 2.5:
func (s *Store) GetInto(key string, dst *domain.Event) error
// → 호출자가 버퍼 제공 → 0 alloc read
```

### Phase 3: Segment Rotation & Compaction

```go
// Segment: maxSegmentSize 도달 시 rotate
//  new segment 생성, old segment는 read-only
// Compaction: 오래된 segment의 dead 레코드 제거

// 구현:
struct Segment {
    id uint64
    createdAt time.Time
    path string
    size int64
}

// Multiple segments:
activeSegment := segments[len(segments)-1]
readSegments := segments[:len(segments)-1]
```

### Phase 3+: Distributed KV (Consistent Hashing)

```
단일 node의 maxKeys 문제 해결:
  multiple nodes + consistent hashing
  → node당 key 수 = total keys / num nodes
  → 선형 scalability

Replication:
  WAL을 다른 노드에 복사 (RAFT 기반)
  → durability + availability
```

---

## Summary

**결정:** Hash Index (Bitcask 모델) 선택

**이유:**
1. **구현 단순성**: ~300 LOC vs LSM 3000 LOC
2. **성능**: O(1) 읽기 vs LSM O(log N)
3. **복구 확실성**: WAL replay는 최고의 보증
4. **Phase 2 적합성**: simple persistence, no range query

**트레이드오프:**
- Range query 불가 (Phase 3+에서 해결)
- Compaction 없음 (WAL 파일 증가)

**검증:**
- 8개 테스트 pass (race-free)
- 예상 성능: Get ~3 μs, Put ~10 μs
- Memory bound: index 크기 = O(unique sources)

**다음 단계:**
- Phase 2 마무리: 성능 벤치마크, 운영 가이드
- Phase 3: Segment rotation + distributed KV
