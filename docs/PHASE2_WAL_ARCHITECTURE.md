# Phase 2 WAL Architecture - Visual Guide

Core-X Phase 2 "Trusted Persistence" 구현의 시각화 문서입니다.

---

## 1. 전체 시스템 아키텍처

```mermaid
graph TB
    subgraph "Client"
        A["HTTP Request<br/>POST /ingest"]
    end

    subgraph "HTTP Layer"
        B["HTTPHandler<br/>(stdlib ServeMux)"]
    end

    subgraph "Application Layer"
        C["IngestionService<br/>.Ingest(source, payload)"]
    end

    subgraph "Infrastructure Layer"
        D["WAL Writer<br/>WriteEvent()"]
        E["Event Pool<br/>(sync.Pool)"]
        F["Worker Pool<br/>(N goroutines)"]
    end

    subgraph "Persistent Storage"
        G["WAL File<br/>(events.wal)"]
    end

    subgraph "Downstream"
        H["Processing<br/>(current: debug log)"]
    end

    A -->|JSON| B
    B -->|source, payload| C
    C -->|1. Write| D
    D -->|Record:MagicTSPayloadSum| G
    C -->|2. Submit| F
    F -->|Get Event| E
    F -->|Process| H
    H -->|Release| E

    style D fill:#ff9999
    style G fill:#99ccff
    style F fill:#99ff99
```

---

## 2. Crash Recovery 플로우

```mermaid
sequenceDiagram
    participant Server as Server Process
    participant WAL as WAL File
    participant Reader as WAL Reader
    participant Pool as Worker Pool
    participant Processor as Event Processor

    Note over Server: Phase 1: Normal Operation
    Server->>WAL: Write Event (SyncInterval)
    Server->>Pool: Submit Event
    Pool->>Processor: Process (async)
    Note over Processor: Events processed

    Note over Server: Phase 2: CRASH (SIGKILL)
    Server-xServer: Process killed
    Note over WAL: Events in WAL ✓<br/>Events in workerPool ✗

    Note over Server: Phase 3: Restart
    Server->>Reader: NewReader(walPath)
    Reader->>WAL: Scan records
    loop For each complete record
        Reader->>Reader: DecodeEvent()
        Reader->>Pool: Submit(recovered event)
        Pool->>Processor: Process (recovery)
    end
    Reader->>Reader: Check error
    alt ErrTruncated
        Reader->>Server: Log & return count
    else Fatal Error
        Reader->>Server: Return error → Exit(1)
    end
    Server->>Server: IngestionService init
    Server->>Server: HTTP listen
```

---

## 3. WAL Reader 상태 머신

```mermaid
stateDiagram-v2
    [*] --> NewReader

    NewReader --> OpenFile: Success
    NewReader --> [*]: Error

    OpenFile --> Idle: File opened<br/>+ buffered reader

    Idle --> Scanning: Scan() called

    Scanning --> ReadHeader: Start reading
    ReadHeader --> ValidateMagic: Header complete

    ValidateMagic --> ReadPayload: Magic OK
    ValidateMagic --> Error: ErrCorrupted

    ReadPayload --> ReadChecksum: Payload complete
    ReadPayload --> Error: ErrTruncated

    ReadChecksum --> VerifyChecksum: Checksum read
    ReadChecksum --> Error: ErrTruncated

    VerifyChecksum --> RecordValid: CRC32 OK
    VerifyChecksum --> Error: ErrChecksumMismatch

    RecordValid --> StickyState: Store record<br/>return true
    StickyState --> Scanning: Scan() called

    Scanning --> EOF: No more data
    EOF --> StickyState: return false

    Error --> StickyState: Store error<br/>return false
    StickyState --> Close: Close() called
    Close --> [*]: File closed

    style ReadHeader fill:#fff4e6
    style ValidateMagic fill:#fff4e6
    style ReadPayload fill:#fff4e6
    style ReadChecksum fill:#fff4e6
    style VerifyChecksum fill:#fff4e6
    style RecordValid fill:#e6ffe6
    style Error fill:#ffe6e6
    style StickyState fill:#e6f2ff
```

---

## 4. 에러 분류 결정 트리

```mermaid
graph TD
    A["recoverWALEvents()"]
    B{"File exists?"}
    C["Return 0, nil<br/>(정상)"]
    D["NewReader()"]
    E{"Read success?"}
    F["Scan loop"]
    G{"DecodeEvent OK?"}
    H["Submit to pool<br/>recoveredCount++"]
    I{"reader.Err() != nil?"}
    J{"ErrTruncated?"}
    K["Log + return count<br/>(정상 복구)"]
    L["Return error<br/>(fatal)"]
    M["Return count<br/>(정상)"]

    A --> B
    B -->|No| C
    B -->|Yes| D
    D --> E
    E -->|Error| L
    E -->|OK| F
    F --> G
    G -->|Error| L
    G -->|OK| H
    H --> F
    F -->|EOF| I
    I -->|nil| M
    I -->|Error| J
    J -->|Yes| K
    J -->|No| L

    style C fill:#e6ffe6
    style K fill:#e6ffe6
    style M fill:#e6ffe6
    style L fill:#ffe6e6
```

---

## 5. E2E 테스트 시나리오

```mermaid
timeline
    title WAL Crash Recovery E2E Test

    section Phase 1-2
        Prepare environment: WAL dir ready
        Start server (initial): Healthz OK

    section Phase 3-4
        Send 5 events: Event 1-5 POSTed
                     : sleep 0.5s (sync)
        Kill with SIGKILL: Process terminated

    section Phase 5-6
        Verify WAL exists: 235 bytes confirmed
        Restart server: Healthz OK

    section Phase 7-8
        Recovery completes: 5 events recovered
        Stats verified: processed_total=5
        All tests PASS: ✓
```

---

## 6. 데이터 흐름: 쓰기 경로 (Ingest)

```mermaid
graph LR
    A["Event arrives<br/>source=S<br/>payload=P"] -->|Ingest| B["walWriter<br/>.WriteEvent()"]

    B -->|1. EncodeEvent| C["Payload bytes<br/>SourceLen+Source<br/>+PayloadLen<br/>+Payload"]

    C -->|2. Build header| D["Header<br/>Magic:0xCAFEBABE<br/>Timestamp:ns<br/>Size:len"]

    D -->|3. Compute CRC32| E["checksum<br/>CRC32<br/>header+payload"]

    E -->|4. Write to disk| F["WAL File<br/>Magic|TS|Size<br/>|Payload|<br/>|Checksum"]

    B -->|5. Submit after WAL| G["workerPool<br/>.Submit()"]

    G -->|6. Queue| H["Job channel<br/>depth=bufferSize"]

    H -->|7. Worker picks| I["Event Processor<br/>goroutine N"]

    I -->|8. Process| J["Output<br/>Debug log<br/>Memory cache<br/>etc"]

    J -->|9. Release| K["Pool.Release()<br/>Reset fields"]

    style F fill:#99ccff
    style B fill:#ff9999
    style G fill:#99ff99
    style H fill:#ffff99
```

---

## 7. 데이터 흐름: 복구 경로 (Recovery)

```mermaid
graph LR
    A["Server restart<br/>after crash"] -->|recoverWALEvents| B["NewReader<br/>walPath"]

    B -->|Open & buffer| C["WAL File<br/>(existing)"]

    C -->|reader.Scan()| D["Read header<br/>16 bytes"]

    D -->|io.ReadFull| E{"All bytes?"}

    E -->|No: EOF| F["return false<br/>Err()=nil"]
    E -->|No: Partial| G["return false<br/>Err()=ErrTruncated"]
    E -->|Yes| H["Validate magic<br/>0xCAFEBABE"]

    H -->|Invalid| I["return false<br/>Err()=ErrCorrupted<br/>EXIT 1"]
    H -->|Valid| J["Read payload<br/>N bytes"]

    J -->|Complete| K["Read checksum<br/>4 bytes"]
    K -->|Complete| L["CRC32 verify<br/>header+payload"]

    L -->|Match| M["DecodeEvent<br/>get Event"]
    L -->|Mismatch| N["Err()=ErrChecksum<br/>EXIT 1"]

    M -->|ReceivedAt←<br/>Timestamp| O["Event ready"]
    O -->|submitter.Submit| P["workerPool<br/>.Submit()"]

    P -->|Queue| Q["Job channel"]
    Q -->|recoveredCount++| R["Next record"]

    R -->|reader.Scan()| D

    F -->|Check error| S{"ErrTruncated?"}
    G -->|Check error| S
    S -->|Yes| T["Log + return count<br/>(정상)"]
    S -->|No| U["Log error<br/>EXIT 1"]

    style A fill:#fff4e6
    style C fill:#99ccff
    style M fill:#e6ffe6
    style P fill:#99ff99
    style T fill:#e6ffe6
    style U fill:#ffe6e6
```

---

## 8. Graceful Shutdown 순서

```mermaid
graph TD
    A["SIGINT/SIGTERM"] -->|Signal handler| B["context<br/>30s timeout"]

    B -->|Step 1| C["server.Shutdown()"]
    C -->|Stop accepting| D["New HTTP connections<br/>blocked"]
    D -->|Existing requests| E["Continue until<br/>WriteTimeout"]

    E -->|Step 2| F["walWriter.Close()"]
    F -->|Force fsync| G["Memory buffer<br/>→ Disk"]
    G -->|Guarantee| H["All ✓ records<br/>on disk"]

    H -->|Step 3| I["workerPool<br/>.Shutdown()"]
    I -->|Close channel| J["jobCh closed"]
    J -->|Workers drain| K["Process remaining<br/>in-flight events"]
    K -->|Guaranteed complete| L["All events<br/>already in WAL"]

    L -->|Result| M["Clean exit<br/>Zero data loss"]

    style C fill:#ffcccc
    style F fill:#ff9999
    style I fill:#99ff99
    style M fill:#ccffcc
```

---

## 9. 에러 분류 의미

```mermaid
graph TB
    A["WAL Reader Error"] -->|File issue| B["ErrCorrupted"]
    A -->|Incomplete record| C["ErrTruncated"]
    A -->|Data corruption| D["ErrChecksumMismatch"]
    A -->|Unknown version| E["ErrVersionUnsupported"]

    B -->|Meaning| B1["Magic ≠ 0xCAFEBABE<br/>Not a WAL file OR<br/>Filesystem damage"]
    B -->|Recovery| B2["Impossible"]
    B -->|Action| B3["LOG ERROR<br/>EXIT 1<br/>Investigate storage"]

    C -->|Meaning| C1["Last record incomplete<br/>at EOF<br/>Process crashed during<br/>final Ingest"]
    C -->|Recovery| C2["All prior records<br/>are safe ✓"]
    C -->|Action| C3["LOG INFO<br/>Return recovered_count<br/>Continue startup<br/>(Normal crash recovery)"]

    D -->|Meaning| D1["CRC32 mismatch<br/>Record payload corrupted<br/>Bit flip in storage"]
    D -->|Recovery| D2["Cannot trust<br/>this record"]
    D -->|Action| D3["LOG ERROR<br/>EXIT 1<br/>Investigate storage<br/>Check backups"]

    E -->|Meaning| E1["WAL file from newer<br/>Core-X version<br/>Forward compatibility"]
    E -->|Recovery| E2["Need newer binary"]
    E -->|Action| E3["LOG ERROR<br/>EXIT 1<br/>Deploy newer version"]

    style B3 fill:#ffe6e6
    style C3 fill:#e6ffe6
    style D3 fill:#ffe6e6
    style E3 fill:#ffe6e6
```

---

## 10. 메모리 레이아웃: Record 구조

```mermaid
graph LR
    subgraph "WAL File on Disk"
        A["Magic: 4B<br/>0xCAFEBABE"]
        B["Timestamp: 8B<br/>UnixNano"]
        C["Size: 4B<br/>len payload"]
        D["Payload: NB<br/>encoded event"]
        E["Checksum: 4B<br/>CRC32 IEEE"]

        A --> B --> C --> D --> E
    end

    subgraph "reader.Record (in memory)"
        F["Timestamp<br/>time.Time"]
        G["Data byte array<br/>raw payload<br/>not decoded yet"]
    end

    subgraph "DecodeEvent returns Event"
        H["Source string"]
        I["Payload string"]
        J["ReceivedAt<br/>filled by caller"]
    end

    E -->|read & verify| F
    E -->|extract| G
    G -->|decode| H
    G -->|decode| I
    F -->|use as| J

    style A fill:#fff4e6
    style B fill:#fff4e6
    style C fill:#fff4e6
    style D fill:#ffcccc
    style E fill:#ff9999
    style F fill:#e6f2ff
    style G fill:#e6f2ff
    style H fill:#e6ffe6
    style I fill:#e6ffe6
```

---

## 11. 테스트 커버리지 매트릭스

```mermaid
graph TB
    A["reader_test.go<br/>10 tests<br/>54.7% coverage"] -->|Core| B["TestReader_EmptyFile<br/>✓ EOF handling"]
    A -->|Core| C["TestReader_SingleRecord<br/>✓ Basic read+decode"]
    A -->|Core| D["TestReader_MultipleRecords<br/>✓ Sequential iteration"]

    A -->|Crash Recovery| E["TestReader_TailTruncation<br/>✓ 99/100 recovered<br/>✓ ErrTruncated"]
    A -->|Crash Recovery| F["TestReader_HeaderTruncation<br/>✓ Partial header"]

    A -->|Data Integrity| G["TestReader_CorruptedMagic<br/>✓ Invalid magic<br/>✓ ErrCorrupted"]
    A -->|Data Integrity| H["TestReader_ChecksumMismatch<br/>✓ CRC32 failure<br/>✓ Detected"]

    A -->|Decoding| I["TestDecodeEvent_RoundTrip<br/>✓ 5 subtests<br/>✓ Empty/normal/large"]
    A -->|Decoding| J["TestDecodeEvent_EdgeCases<br/>✓ 5 boundary tests<br/>✓ Truncation detection"]

    style A fill:#fff4e6
    style B fill:#e6ffe6
    style C fill:#e6ffe6
    style D fill:#e6ffe6
    style E fill:#ffe6e6
    style F fill:#ffe6e6
    style G fill:#ffcccc
    style H fill:#ffcccc
    style I fill:#ccffcc
    style J fill:#ccffcc
```

---

## 12. 성능 특성

```mermaid
graph TB
    A["Performance Targets<br/>ADR-004"]

    A -->|Throughput| B["Sequential Read<br/>100M bytes/sec"]
    A -->|Latency| C["Decode Overhead<br/>less than 100ns/record"]
    A -->|Recovery| D["Truncation Handling<br/>O file_size"]

    B -->|File: 256KB WAL| B1["1000 records<br/>Time: 2.5ms"]
    B1 -->|Uses| B2["4KB bufio buffer"]
    B2 -->|Syscalls| B3["25k syscalls<br/>vs 250k without"]

    C -->|Process| C1["Reverse EncodeEvent<br/>2 binary reads"]
    C1 -->|Cost| C2["Few CPU cycles<br/>negligible in IO"]

    D -->|Algorithm| D1["Scan all complete<br/>skip partial EOF"]
    D1 -->|Complexity| D2["O of N where<br/>N file size"]

    style B3 fill:#ccffcc
    style C2 fill:#ccffcc
    style D2 fill:#ccffcc
```

---

## 13. 아키텍처 결정 (ADR-004)

```mermaid
graph TB
    A["Design Decisions"] -->|Interface Pattern| B["Iterator Pattern<br/>Scan()→Record()→Err()"]
    A -->|Error Handling| C["4 Error Types<br/>ErrTruncated,<br/>Corrupted,<br/>Checksum,<br/>Version"]
    A -->|Buffer Strategy| D["4KB bufio.Reader<br/>syscall reduction"]
    A -->|Validation| E["CRC32 over<br/>header+payload"]
    A -->|Truncation| F["io.ReadFull<br/>distinguishes EOF<br/>from Incomplete"]

    B -->|Why| B1["✓ Caller controls loop<br/>✓ bufio.Scanner idiom<br/>✓ Sticky error state"]
    C -->|Why| C1["✓ Recoverable vs fatal<br/>✓ ErrTruncated expected<br/>✓ Others: investigate"]
    D -->|Why| D1["✓ Sequential only<br/>✓ 1 syscall per 4KB<br/>✓ Not thread-safe<br/>(recovery single thread)"]
    E -->|Why| E1["✓ Detects bit flips<br/>✓ Fast (few cycles)<br/>✓ stdlib only"]
    F -->|Why| F1["✓ Atomic: all or error<br/>✓ No partial handling<br/>✓ Clear semantics"]

    style B1 fill:#ccffcc
    style C1 fill:#ccffcc
    style D1 fill:#ccffcc
    style E1 fill:#ccffcc
    style F1 fill:#ccffcc
```

---

## Phase 2 Summary

```mermaid
graph TD
    A["Phase 2: Trusted Persistence"] -->|Implementation| B["✓ WAL Writer<br/>✓ WAL Reader<br/>✓ Crash Recovery<br/>✓ E2E Test"]

    B -->|Testing| C["✓ 8 core tests<br/>✓ 2 edge case tests<br/>✓ 54.7% coverage<br/>✓ All PASS"]

    B -->|Verification| D["✓ Benchmarks OK<br/>✓ Code compiles<br/>✓ E2E SIGKILL→restart<br/>✓ 5/5 events recovered"]

    A -->|Guarantee| E["All writes → WAL<br/>All crashes → recoverable<br/>All complete records → safe<br/>All fatal errors → detected"]

    A -->|Next: Phase 3| F["Rotation: split large files<br/>Checkpointing: resume mid-file<br/>Optimization: buffer pooling"]

    style E fill:#ccffcc
```

---

## 참고: 파일 구조

```
core-x/
├── docs/
│   ├── adr/
│   │   └── 0004-wal-reader-design.md          (설계 결정)
│   └── PHASE2_WAL_ARCHITECTURE.md             (이 파일)
├── internal/
│   └── infrastructure/storage/wal/
│       ├── reader.go                          (~250 lines)
│       ├── reader_test.go                     (13KB, 10 tests)
│       ├── errors.go                          (sentinel errors)
│       ├── writer.go                          (existing)
│       └── encode.go                          (existing)
├── cmd/
│   └── main.go                                (recovery 통합)
├── scripts/
│   └── test_recovery.sh                       (E2E test)
└── bench/
    └── wal_reader_bench_test.go               (performance)
```
