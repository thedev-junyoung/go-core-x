# ADR-004: WAL Reader — Iterator 패턴 및 Tail Truncation 복구

- **상태**: 승인됨
- **작성일**: 2026-04-08
- **결정자**: Core-X Principal Architect

---

## 문맥

### Phase 2 요구사항

Core-X Phase 2 목표: Trusted Persistence (신뢰성 있는 영속성). WAL Writer는 완료되었으나, 크래시 후 데이터를 다시 읽을 수 없는 상태.

**문제**: Reader 없이는 WAL이 쓰기 전용이다. 크래시 복구 및 이벤트 재생이 불가능하다.

**요구사항**:
- WAL을 처음부터 순차적으로 읽기
- Tail truncation (파일 끝의 불완전한 레코드) 감지 및 우아한 복구
- 데이터 손상 감지 (매직 불일치, 체크섬 실패)
- 바이너리 형식에서 Event 객체 복원
- 크래시 복구 지원: 서버 시작 시 WAL에서 이벤트 재생

### 성능 문맥

복구 경로 (핫 경로 아님):
- 정확성 > 처리량
- 싱글 스레드 (복구는 서버 시작 시, 수신 전에 실행)
- 할당 예산 제약 없음 (HTTP 요청 핸들러와 달리)
- 처리량 목표: 100M bytes/sec (복구 충분)

### 기술 제약사항

- stdlib만 사용 (외부 라이브러리 금지)
- Writer와 프로토콜 호환성 (writer.go 변경 불가)
- bufio.Reader 친화적 (순차 읽기)
- CRC32 검증이 Writer 계산과 일치해야 함

---

## 결정사항

### 1. Iterator 패턴 (bufio.Scanner 스타일)

Reader는 `bufio.Scanner`와 동일한 패턴으로 구현하여 친숙성과 정확성을 확보한다.

```go
r, err := wal.NewReader(walPath)
if err != nil { ... }
defer r.Close()

for r.Scan() {
    rec := r.Record()  // 현재 레코드
    e, err := wal.DecodeEvent(rec.Data)
    if err != nil { /* 디코딩 에러 처리 */ }
    // 이벤트 처리
}
if err := r.Err(); err != nil && !errors.Is(err, wal.ErrTruncated) {
    // 치명적 에러 (tail truncation 제외)
}
```

**근거**:
- 호출자가 루프 흐름 제어 (조기 탈출 가능)
- 에러 처리 명시적 (호출자가 Err() 호출)
- 친숙한 Go 관용구 (Scanner, bufio.Reader)
- Iterator를 일시 중지, 재개, 중단할 수 있음

### 2. 에러 분류

네 가지 별개의 에러 타입을 서로 다른 시나리오에 정의:

```go
var (
    // Tail truncation: 파일 끝에서 레코드 불완전
    // 복구: 안전, 이전 레코드는 유효
    ErrTruncated = errors.New("wal: record truncated at end of file")

    // 파일 손상: 매직 불일치
    // 복구: 치명적, 데이터 손실 시사
    ErrCorrupted = errors.New("wal: magic number mismatch, file may be corrupted")

    // 데이터 무결성: 체크섬 실패
    // 복구: 치명적, payload 손상 시사
    ErrChecksumMismatch = errors.New("wal: checksum mismatch")

    // 프로토콜 진화: 알려지지 않은 버전
    // 복구: 업그레이드된 바이너리로 재시도
    ErrVersionUnsupported = errors.New("wal: unsupported protocol version")
)
```

**근거**:
- ErrTruncated는 정상 종료 시 예상됨 (마지막 쓰기가 중단됨)
- ErrCorrupted는 파일시스템 손상을 시사 (즉시 중단)
- ErrChecksumMismatch는 payload 손상을 시사 (즉시 중단)
- ErrVersionUnsupported는 향후 프로토콜 진화를 가능하게 함 (업그레이드된 바이너리로 재시도)

호출자는 구분: `errors.Is(err, wal.ErrTruncated)` → 복구 가능, 나머지 → 치명적.

### 3. 버퍼된 순차 읽기

시스템콜 감소를 위해 4KB 버퍼가 있는 `bufio.Reader` 사용.

```go
type Reader struct {
    file   *os.File
    br     *bufio.Reader  // 4KB 버퍼
    record Record
    err    error
}

r.br = bufio.NewReaderSize(f, 4*1024)  // 4KB 청크로 읽기
```

**근거**:
- 순차 접근 패턴 (처음부터 전체 파일 읽음)
- 시스템콜 최소화: 연산당 1개 시스템콜 vs 4KB당 1개 시스템콜
- 표준 Go 라이브러리, 입증된 성능
- 잠금 없음 (싱글 스레드 복구 경로)

### 4. Record 구조

```go
type Record struct {
    Timestamp int64   // WAL 헤더의 UnixNano
    Data      []byte  // Payload (Event 디코딩 전)
}
```

**설계**:
- Timestamp는 WAL 레코드에서 보존됨 (Event.ReceivedAt 복원용)
- Data는 원본 payload (아직 Event로 디코딩되지 않음)
- 호출자가 디코딩 복잡성을 소유

### 5. 우아한 Truncation 복구

Tail truncation 감지 핵심 알고리즘:

```
헤더 읽기 (16 bytes):
  - 0 bytes 읽음 → EOF (정상 종료)
  - 1-15 bytes 읽음 → ErrTruncated (불완전한 헤더)
  - 16+ bytes → 계속

Payload 읽기 (N bytes):
  - 모든 N bytes 읽음 → 계속
  - < N bytes → ErrTruncated (불완전한 payload)

체크섬 읽기 (4 bytes):
  - 4 bytes 읽음 → 계속
  - < 4 bytes → ErrTruncated (불완전한 체크섬)

체크섬 검증:
  - 일치 → Record 반환
  - 불일치 → ErrChecksumMismatch
```

**구현**: `io.ReadFull()`을 사용하면 EOF와 UnexpectedEOF를 구분함.

```go
header := make([]byte, RecordHeaderSize)
_, err := io.ReadFull(r.br, header)
if err == io.EOF {
    // 정상적인 파일 끝
    return Record{}, io.EOF
}
if err == io.ErrUnexpectedEOF {
    // 불완전한 헤더 → truncation
    return Record{}, ErrTruncated
}
if err != nil {
    return Record{}, err
}
// payload 계속 읽기...
```

**근거**:
- `io.ReadFull`은 원자적: 모든 bytes 또는 에러
- 부분 읽기를 처리할 필요 없음
- "끝"과 "불완전"을 명확하게 구분

### 6. CRC32 검증

체크섬을 header + payload에 대해 계산 (Writer와 동일).

```go
// Writer 계산:
record := append(header, payload...)
checksum := crc32.ChecksumIEEE(record)

// Reader 검증:
computed := crc32.ChecksumIEEE(header)
computed = crc32.Update(computed, crc32.IEEETable, payload)
if computed != stored {
    return ErrChecksumMismatch
}
```

**근거**:
- 체크섬이 header와 payload를 모두 검증
- 어느 섹션이든 비트 플립을 감지
- CRC32는 빠름 (현대 CPU에서 ~3 사이클/byte)

### 7. Event 디코딩

EncodeEvent 형식의 역함수.

```go
// EncodeEvent 형식:
// [SourceLen:2] [Source:N] [PayloadLen:2] [Payload:M]

func DecodeEvent(data []byte) (*domain.Event, error) {
    offset := 0
    sourceLen := binary.BigEndian.Uint16(data[offset:])
    offset += 2
    source := string(data[offset : offset+int(sourceLen)])
    offset += int(sourceLen)
    payloadLen := binary.BigEndian.Uint16(data[offset:])
    offset += 2
    payload := string(data[offset : offset+int(payloadLen)])

    return &domain.Event{
        Source: source,
        Payload: payload,
    }, nil
}
```

**제한사항**:
- ReceivedAt은 WAL Record.Timestamp에서 복원됨 (호출자 책임)
- Source/Payload 길이는 각각 65535 bytes로 제한됨 (uint16)

---

## 결과

### 성능

**처리량**: ~100M bytes/sec의 순차 읽기 (일반적인 SSD 지속성능).
- 4KB 버퍼 → ~25k 시스템콜/sec (관리 가능)
- CRC32 인라인 검증
- 레코드당 단일 malloc (payload)

**메모리**: 재생 중 O(max_record_size).

**할당**:
- 레코드당: 1 할당 (payload 버퍼)
- 객체 풀링 없음 (복구 경로, 레이턴시 critical 아님)

### 신뢰성

**데이터 무결성**:
- CRC32가 우발적 손상을 포착
- 의도적 변조에는 보호하지 않음 (필요하면 HMAC 사용, Phase 4)

**크래시 복구**:
- 모든 크래시 유형 후 안전한 재시작:
  - 정상 종료: 불완전한 레코드 없음
  - SIGKILL: tail truncation 감지, 마지막 완전한 레코드까지 복구
  - 디스크 에러: 손상 감지, 중단 및 경고

**데이터 손실 없음** (주의사항 포함):
- 모든 완전한 레코드는 보존됨
- Tail의 불완전한 레코드는 버려짐 (수용 가능, 보통 비어있거나 부분적 진행 중 데이터)

### 유지보수성

- Go 개발자에게 친숙한 Iterator 패턴
- 에러는 명시적 타입 (테스트 가능, 문자열이 아님)
- 싱글 스레드 경로: 동시성 복잡성 없음
- 외부 의존성 없음

### 검증 방법

아래의 모니터링/검증 섹션 참조.

---

## 검토된 대안

### 1. 채널 기반 Iterator

```go
// 대안: goroutine이 레코드를 읽고 채널로 전송
for rec := range r.ReadChannel() {
    // 처리
}
```

**거절 이유**:
- Goroutine 생명주기 관리 어려움 (defer로 정리, panic recovery)
- 채널 닫기 의미론 (close vs stop on error)
- 명확한 에러 처리 경로 없음 (버퍼된 채널? 에러 채널 select?)
- Go 관용구는 순차 반복에 `bufio.Scanner`, 채널이 아님

### 2. 콜백 기반 읽기

```go
// 대안: reader가 각 레코드에 대해 콜백 호출
r.ReadAll(func(rec Record) error { ... })
```

**거절 이유**:
- 호출자가 루프를 제어할 수 없음 (조기 탈출 불가)
- 에러 처리 불명확 (콜백 에러가 읽기를 중단하는가?)
- 테스트 어려움 (콜백 모킹 필요)

### 3. 버퍼된 채널 큐

```go
// 대안: 동시 읽기 goroutine과 내부 버퍼링
jobCh := make(chan Record, 100)
go r.readAsync(jobCh)
for rec := range jobCh { ... }
```

**거절 이유**:
- 싱글 스레드 복구 경로에 불필요한 복잡성
- Goroutine 생명주기 관리 (에러 시 reader goroutine이 언제 종료되는가?)
- 복구는 레이턴시 critical이 아니므로 순차가 좋음

### 4. 메모리 맵 파일 읽기

```go
// 대안: mmap(2)으로 zero-copy 읽기
data, _ := syscall.Mmap(fd, 0, filesize, syscall.PROT_READ, ...)
```

**거절 이유**:
- WAL 파일이 매우 클 수 있음 (프로덕션에서 GB)
- 큰 파일의 주소 공간 오버헤드
- 이식성 없음 (Windows는 다른 의미론)
- 조기 최적화 (벤치마킹이 I/O가 병목인지 알려줄 것)

### 5. 병렬 다중 파일 읽기

```go
// 대안: WAL을 세그먼트로 분할, 병렬 읽기
readers := make([]*Reader, numSegments)
```

**Phase 3로 연기**:
- 단일 파일 읽기가 현재 충분히 빠름
- 병렬화는 WAL rotation이 구현되었을 때만 필요
- Phase 2 벤치마크에서 병목을 보인 후 재검토

---

## 관련 결정사항

- [ADR-001: Pure Go Standard Library](0001-use-pure-go-standard-library.md) — Reader는 stdlib만 사용 (bufio, io, encoding/binary)
- [ADR-002: Worker Pool & Sync.Pool](0002-worker-pool-and-sync-pool-for-performance.md) — Reader는 핫 경로에서 분리; 복구는 할당을 감당할 수 있음
- [ADR-003: Clean Architecture + Zero-Allocation](0003-clean-architecture-with-zero-allocation.md) — Reader는 Infrastructure 계층, Application으로 domain.Event 반환

---

## 모니터링 / 검증

### 벤치마크 목표

```bash
# 처리량: 전체 WAL 파일 순차 읽기
go test ./bench/... -bench=BenchmarkReader_Sequential -benchmem

# 목표: > 100M bytes/sec
# 베이스라인: 1M 이벤트 파일 (레코드당 56 bytes 헤더 + 200 byte payload = 256 bytes/record)
#           ~4k 레코드, < 50ms 내에 완료해야 함
```

```bash
# RoundTrip: EncodeEvent → Write → Read → DecodeEvent → 원본
go test ./bench/... -bench=BenchmarkRoundTrip_WriteThenRead -benchmem

# 목표: < 100 ns/record 오버헤드 (디코딩 레이턴시), alloc/op < 1
```

```bash
# 복구: tail truncation 시뮬레이션
go test ./bench/... -bench=BenchmarkReader_TruncationRecovery -benchmem

# 목표: 마지막 레코드가 자려져도 N-1개 레코드 복구
```

### 검증 테스트

| 테스트 | 목적 | 통과 기준 |
|--------|------|----------|
| `TestReader_EmptyFile` | 정상 EOF 처리 | Scan() false, Err() nil |
| `TestReader_SingleRecord` | 단일 레코드 읽기 | 데이터 일치 |
| `TestReader_MultipleRecords` | 순차 읽기 | 모든 N개 레코드 올바르게 읽음 |
| `TestReader_TailTruncation` | 크래시 복구 | ErrTruncated 반환, 이전 레코드 복구 |
| `TestReader_HeaderTruncation` | 불완전한 헤더 | 0-15 bytes에서 ErrTruncated |
| `TestReader_CorruptedMagic` | 매직 검증 | 잘못된 매직에서 ErrCorrupted |
| `TestReader_ChecksumMismatch` | CRC32 검증 | 비트 플립에서 ErrChecksumMismatch |
| `TestDecodeEvent_RoundTrip` | Event 대칭성 | EncodeEvent(e) → DecodeEvent(bytes) == e |
| `TestDecodeEvent_EdgeCases` | 경계 조건 | Empty source, empty payload, 둘 다 empty |

### E2E 검증

```bash
# 크래시 시뮬레이션: 서버 시작, N개 이벤트 전송, kill -9, 재시작
# 검증: WAL 파일에서 모든 N개 이벤트 복구됨
# 데이터 손실 없음 (크래시 순간의 불완전한 이벤트 제외)
```

---

## 구현 체크리스트

- [x] errors.go: 4개 sentinel 에러 정의
- [x] reader.go: Reader struct + 메서드 구현
  - [x] NewReader(path)
  - [x] Scan() bool
  - [x] Record() Record
  - [x] Err() error
  - [x] Close() error
  - [x] readRecord() (private)
  - [x] validateMagic(magic uint32) error (private)
- [x] DecodeEvent(data []byte) (*domain.Event, error)
- [x] bench/wal_reader_bench_test.go: 벤치마크
- [x] internal/infrastructure/storage/wal/reader_test.go: 검증 테스트
- [x] cmd/main.go: 시작 시 replay 통합 (Phase 2 최종)

---

## 향후 개선사항 (Phase 3+)

- **WAL Rotation**: 큰 파일을 세그먼트로 분할, 병렬 읽기
- **Reader Checkpointing**: "마지막 좋은 offset" 추적, 중간부터 재개
- **CRC32 대신 HMAC**: 의도적 변조에 대한 방어
- **Compressed Records**: 저장소가 병목이면 WAL 파일 크기 감소
- **Streaming Decode**: 매우 큰 Event payload용 (Phase 3)
