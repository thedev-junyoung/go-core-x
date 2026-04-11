# Phase 3b: Basic Replication (Primary → Replica Async WAL Streaming) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Primary 노드의 WAL을 Replica 노드에 비동기 gRPC server-side streaming으로 복제하여, Primary 장애 시에도 데이터를 보존한다.

**Architecture:** gRPC server-side streaming으로 Primary가 WAL raw bytes를 push. Replica는 수신한 bytes를 local WAL에 append + fsync. ACK는 reconnect 시 `start_offset`으로 대체 (PostgreSQL WAL receiver 패턴). Compaction 감지를 위해 `wal.Writer`에 `CompactionNotify()` 추가.

**Tech Stack:** Go stdlib + `google.golang.org/grpc` (기존 의존성) + Protobuf

---

## File Map

| 파일 | 액션 | 책임 |
|------|------|------|
| `proto/ingest.proto` | 수정 | `ReplicationService.StreamWAL` RPC 추가 |
| `proto/pb/` | 재생성 | `protoc` 재실행 |
| `internal/infrastructure/cluster/role.go` | 신규 | `Role` enum + 환경 변수 파싱 |
| `internal/infrastructure/storage/wal/writer.go` | 수정 | `CompactionNotify() <-chan struct{}` 추가 |
| `internal/infrastructure/replication/lag.go` | 신규 | `ReplicationLag` — atomic int64 기반 lag 추적 |
| `internal/infrastructure/replication/streamer.go` | 신규 | Primary WAL tail-follow → gRPC push |
| `internal/infrastructure/replication/receiver.go` | 신규 | Replica WALEntry 수신 → local WAL append + fsync |
| `internal/infrastructure/replication/server.go` | 신규 | gRPC `ReplicationService` 서버 구현 |
| `internal/infrastructure/replication/client.go` | 신규 | Replica → Primary 연결 + reconnect loop |
| `cmd/main.go` | 수정 | replication 컴포넌트 wiring |

---

## Task 1: Proto 확장 — ReplicationService 추가

**Files:**
- Modify: `proto/ingest.proto`
- Regenerate: `proto/pb/` (protoc 실행)

- [ ] **Step 1: proto 파일에 ReplicationService 추가**

`proto/ingest.proto` 끝에 추가:

```protobuf
// ReplicationService handles WAL streaming from primary to replica.
service ReplicationService {
  // StreamWAL opens a long-lived stream. Primary pushes WALEntry records
  // starting from the replica's last acknowledged offset.
  rpc StreamWAL(StreamWALRequest) returns (stream WALEntry);
}

// StreamWALRequest is sent once by the replica to open the stream.
message StreamWALRequest {
  string replica_id   = 1;
  int64  start_offset = 2;
}

// WALEntry is a single replicated WAL record.
message WALEntry {
  int64 offset      = 1;
  bytes raw_data    = 2;
  int64 record_size = 3;
}
```

- [ ] **Step 2: protoc 재실행**

```bash
protoc \
  --go_out=./proto/pb \
  --go_opt=paths=source_relative \
  --go-grpc_out=./proto/pb \
  --go-grpc_opt=paths=source_relative \
  proto/ingest.proto
```

Expected: `proto/pb/ingest.pb.go` 및 `proto/pb/ingest_grpc.pb.go` 재생성. `ReplicationServiceClient`, `ReplicationServiceServer` 인터페이스 포함.

- [ ] **Step 3: 컴파일 확인**

```bash
go build ./...
```

Expected: 빌드 성공, 에러 없음.

- [ ] **Step 4: 커밋**

```bash
git add proto/ingest.proto proto/pb/ingest.pb.go proto/pb/ingest_grpc.pb.go
git commit -m "feat(proto): add ReplicationService.StreamWAL for Phase 3b"
```

---

## Task 2: cluster/role.go — 역할 결정 파싱

**Files:**
- Create: `internal/infrastructure/cluster/role.go`

- [ ] **Step 1: 테스트 작성**

Create `internal/infrastructure/cluster/role_test.go`:

```go
package cluster_test

import (
	"os"
	"testing"

	"github.com/junyoung/core-x/internal/infrastructure/cluster"
)

func TestRoleFromEnv(t *testing.T) {
	tests := []struct {
		env      string
		want     cluster.Role
		wantErr  bool
	}{
		{"primary", cluster.RolePrimary, false},
		{"replica", cluster.RoleReplica, false},
		{"",        cluster.RoleStandalone, false},
		{"invalid", cluster.RoleStandalone, true},
	}

	for _, tc := range tests {
		t.Run(tc.env, func(t *testing.T) {
			os.Setenv("CORE_X_ROLE", tc.env)
			defer os.Unsetenv("CORE_X_ROLE")

			got, err := cluster.RoleFromEnv()
			if (err != nil) != tc.wantErr {
				t.Fatalf("RoleFromEnv() err = %v, wantErr %v", err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Errorf("RoleFromEnv() = %v, want %v", got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: 테스트 실패 확인**

```bash
go test ./internal/infrastructure/cluster/... -run TestRoleFromEnv -v
```

Expected: FAIL — `cluster.Role` undefined.

- [ ] **Step 3: role.go 구현**

Create `internal/infrastructure/cluster/role.go`:

```go
package cluster

import (
	"fmt"
	"os"
)

// Role represents the replication role of this node.
type Role int

const (
	// RoleStandalone is the default single-node mode (no replication).
	RoleStandalone Role = iota
	// RolePrimary owns WAL and streams it to replicas.
	RolePrimary
	// RoleReplica receives WAL stream from a primary.
	RoleReplica
)

func (r Role) String() string {
	switch r {
	case RolePrimary:
		return "primary"
	case RoleReplica:
		return "replica"
	default:
		return "standalone"
	}
}

// RoleFromEnv reads CORE_X_ROLE and returns the corresponding Role.
// Returns RoleStandalone if the variable is unset or empty.
// Returns an error if the value is set but unrecognized.
func RoleFromEnv() (Role, error) {
	v := os.Getenv("CORE_X_ROLE")
	switch v {
	case "":
		return RoleStandalone, nil
	case "primary":
		return RolePrimary, nil
	case "replica":
		return RoleReplica, nil
	default:
		return RoleStandalone, fmt.Errorf("cluster: unknown role %q (expected primary|replica)", v)
	}
}
```

- [ ] **Step 4: 테스트 통과 확인**

```bash
go test ./internal/infrastructure/cluster/... -run TestRoleFromEnv -v
```

Expected: PASS.

- [ ] **Step 5: 커밋**

```bash
git add internal/infrastructure/cluster/role.go internal/infrastructure/cluster/role_test.go
git commit -m "feat(cluster): add Role enum and RoleFromEnv for static primary/replica assignment"
```

---

## Task 3: wal.Writer — CompactionNotify() 추가

**Files:**
- Modify: `internal/infrastructure/storage/wal/writer.go`

- [ ] **Step 1: 테스트 작성**

`internal/infrastructure/storage/wal/writer_compaction_test.go` 생성:

```go
package wal_test

import (
	"os"
	"testing"
	"time"

	"github.com/junyoung/core-x/internal/infrastructure/storage/wal"
)

func TestCompactionNotify(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "wal-compaction-*.wal")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	w, err := wal.NewWriter(wal.Config{
		Path:       f.Name(),
		SyncPolicy: wal.SyncNever,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	ch1 := w.CompactionNotify()

	// Write something and simulate a compaction swap (open a new temp file).
	newFile, err := os.CreateTemp(t.TempDir(), "wal-new-*.wal")
	if err != nil {
		t.Fatal(err)
	}

	err = w.RunExclusiveSwap(func() (*os.File, int64, error) {
		return newFile, 0, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// ch1 should be closed (signaled) after swap.
	select {
	case <-ch1:
		// expected
	case <-time.After(100 * time.Millisecond):
		t.Fatal("CompactionNotify channel was not closed after RunExclusiveSwap")
	}

	// A new channel should be returned after swap.
	ch2 := w.CompactionNotify()
	if ch1 == ch2 {
		t.Fatal("CompactionNotify should return a new channel after swap")
	}
}
```

- [ ] **Step 2: 테스트 실패 확인**

```bash
go test ./internal/infrastructure/storage/wal/... -run TestCompactionNotify -v
```

Expected: FAIL — `CompactionNotify` method not found.

- [ ] **Step 3: Writer 구조체에 필드 추가 + CompactionNotify() 구현**

`internal/infrastructure/storage/wal/writer.go`에서 `Writer` struct에 필드 추가:

```go
// compactionCh is closed by RunExclusiveSwap to notify listeners (e.g. replication
// streamer) that the WAL file has been atomically swapped. A new channel is
// assigned after each swap so that listeners can re-subscribe.
compactionCh chan struct{}
compactionMu sync.Mutex
```

`NewWriter` 함수에서 초기화 추가 (`w := &Writer{...}` 블록 내):

```go
compactionCh: make(chan struct{}),
```

`CompactionNotify()` 메서드 추가:

```go
// CompactionNotify returns a channel that is closed when RunExclusiveSwap
// completes a WAL file swap. A new channel is issued after each swap, so
// callers must re-call CompactionNotify after receiving the signal.
func (w *Writer) CompactionNotify() <-chan struct{} {
	w.compactionMu.Lock()
	defer w.compactionMu.Unlock()
	return w.compactionCh
}
```

`RunExclusiveSwap` 메서드 끝 부분 수정 (old.Close() 이전):

```go
// Signal compaction listeners and replace the channel.
w.compactionMu.Lock()
oldCh := w.compactionCh
w.compactionCh = make(chan struct{})
w.compactionMu.Unlock()
close(oldCh)

old := w.file
w.file = newFile
w.sizeTracker.Store(newSize)
return old.Close()
```

- [ ] **Step 4: 테스트 통과 확인**

```bash
go test ./internal/infrastructure/storage/wal/... -v
```

Expected: 전체 PASS.

- [ ] **Step 5: 커밋**

```bash
git add internal/infrastructure/storage/wal/writer.go internal/infrastructure/storage/wal/writer_compaction_test.go
git commit -m "feat(wal): add CompactionNotify to signal WAL file swap to replication streamer"
```

---

## Task 4: replication/lag.go — Lag 모니터링

**Files:**
- Create: `internal/infrastructure/replication/lag.go`

- [ ] **Step 1: 테스트 작성**

Create `internal/infrastructure/replication/lag_test.go`:

```go
package replication_test

import (
	"testing"

	"github.com/junyoung/core-x/internal/infrastructure/replication"
)

func TestReplicationLag(t *testing.T) {
	lag := replication.NewReplicationLag()

	if got := lag.Bytes(); got != 0 {
		t.Fatalf("initial lag = %d, want 0", got)
	}

	lag.UpdatePrimary(1000)
	lag.UpdateReplica(600)

	if got := lag.Bytes(); got != 400 {
		t.Errorf("lag = %d, want 400", got)
	}

	// lag cannot go negative (replica caught up past primary snapshot)
	lag.UpdateReplica(1200)
	if got := lag.Bytes(); got < 0 {
		t.Errorf("lag should not be negative, got %d", got)
	}
}
```

- [ ] **Step 2: 테스트 실패 확인**

```bash
go test ./internal/infrastructure/replication/... -run TestReplicationLag -v
```

Expected: FAIL — package not found.

- [ ] **Step 3: lag.go 구현**

Create `internal/infrastructure/replication/lag.go`:

```go
// Package replication implements Phase 3b WAL streaming replication.
package replication

import "sync/atomic"

// ReplicationLag tracks the byte offset difference between primary and replica.
// A higher value means the replica is further behind.
type ReplicationLag struct {
	primaryOffset atomic.Int64
	replicaOffset atomic.Int64

	reconnectCount atomic.Int64
}

// NewReplicationLag creates a new ReplicationLag with all counters at zero.
func NewReplicationLag() *ReplicationLag {
	return &ReplicationLag{}
}

// UpdatePrimary sets the latest offset streamed by the primary.
func (l *ReplicationLag) UpdatePrimary(offset int64) {
	l.primaryOffset.Store(offset)
}

// UpdateReplica sets the latest offset persisted by the replica.
func (l *ReplicationLag) UpdateReplica(offset int64) {
	l.replicaOffset.Store(offset)
}

// Bytes returns the current replication lag in bytes.
// A negative result means replica reported a higher offset (race) — treat as 0.
func (l *ReplicationLag) Bytes() int64 {
	v := l.primaryOffset.Load() - l.replicaOffset.Load()
	if v < 0 {
		return 0
	}
	return v
}

// IncReconnect increments the reconnect counter.
func (l *ReplicationLag) IncReconnect() {
	l.reconnectCount.Add(1)
}

// ReconnectCount returns the total number of replica reconnections.
func (l *ReplicationLag) ReconnectCount() int64 {
	return l.reconnectCount.Load()
}
```

- [ ] **Step 4: 테스트 통과 확인**

```bash
go test ./internal/infrastructure/replication/... -run TestReplicationLag -v
```

Expected: PASS.

- [ ] **Step 5: 커밋**

```bash
git add internal/infrastructure/replication/lag.go internal/infrastructure/replication/lag_test.go
git commit -m "feat(replication): add ReplicationLag for monitoring offset gap between primary and replica"
```

---

## Task 5: replication/streamer.go — Primary WAL tail-follow

**Files:**
- Create: `internal/infrastructure/replication/streamer.go`

- [ ] **Step 1: 테스트 작성**

Create `internal/infrastructure/replication/streamer_test.go`:

```go
package replication_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/junyoung/core-x/internal/infrastructure/replication"
	"github.com/junyoung/core-x/internal/infrastructure/storage/wal"
)

func TestStreamer_SendsRecords(t *testing.T) {
	dir := t.TempDir()
	walPath := dir + "/test.wal"

	// Write 3 records to WAL.
	w, err := wal.NewWriter(wal.Config{Path: walPath, SyncPolicy: wal.SyncNever})
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("hello"))
	w.Write([]byte("world"))
	w.Write([]byte("bye"))
	w.Sync()
	w.Close()

	// Open WAL file for reading.
	f, err := os.Open(walPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	lag := replication.NewReplicationLag()
	streamer := replication.NewStreamer(f, w.CompactionNotify, lag, 5*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	var entries []replication.RawEntry
	err = streamer.Stream(ctx, 0, func(e replication.RawEntry) error {
		entries = append(entries, e)
		if len(entries) == 3 {
			cancel() // got all, stop
		}
		return nil
	})

	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d (err=%v)", len(entries), err)
	}
	// Verify offsets are monotonically increasing.
	for i := 1; i < len(entries); i++ {
		if entries[i].Offset <= entries[i-1].Offset {
			t.Errorf("entries[%d].Offset=%d not > entries[%d].Offset=%d",
				i, entries[i].Offset, i-1, entries[i-1].Offset)
		}
	}
}
```

- [ ] **Step 2: 테스트 실패 확인**

```bash
go test ./internal/infrastructure/replication/... -run TestStreamer_SendsRecords -v
```

Expected: FAIL — `replication.Streamer` undefined.

- [ ] **Step 3: streamer.go 구현**

Create `internal/infrastructure/replication/streamer.go`:

```go
package replication

import (
	"context"
	"encoding/binary"
	"io"
	"os"
	"time"

	"github.com/junyoung/core-x/internal/infrastructure/storage/wal"
)

// RawEntry is a single raw WAL record to be sent to a replica.
type RawEntry struct {
	Offset     int64
	RawData    []byte
	RecordSize int64
}

// Streamer tails a WAL file and delivers RawEntry values to a callback.
// It is used by the gRPC ReplicationService server to push WAL entries to replicas.
type Streamer struct {
	walFile         *os.File
	compactionNotify func() <-chan struct{}
	lag             *ReplicationLag
	pollInterval    time.Duration
}

// NewStreamer creates a Streamer for the given open WAL file.
// compactionNotify is wal.Writer.CompactionNotify — called each time to get
// the current notification channel.
func NewStreamer(walFile *os.File, compactionNotify func() <-chan struct{}, lag *ReplicationLag, pollInterval time.Duration) *Streamer {
	return &Streamer{
		walFile:          walFile,
		compactionNotify: compactionNotify,
		lag:              lag,
		pollInterval:     pollInterval,
	}
}

// Stream reads WAL records starting at startOffset and calls fn for each one.
// It tail-follows the file: on EOF it polls until new data arrives or ctx is done.
// If a compaction swap is signaled, Stream reopens the WAL file from offset 0.
//
// fn returning a non-nil error terminates Stream immediately with that error.
func (s *Streamer) Stream(ctx context.Context, startOffset int64, fn func(RawEntry) error) error {
	f := s.walFile
	offset := startOffset
	compactionCh := s.compactionNotify()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-compactionCh:
			// WAL file was atomically swapped by compaction.
			// Reopen the file (walFile path) from the beginning.
			newF, err := os.Open(f.Name())
			if err != nil {
				return err
			}
			if f != s.walFile {
				f.Close() // close previously reopened handle
			}
			f = newF
			offset = 0
			compactionCh = s.compactionNotify()
			continue
		default:
		}

		rec, err := wal.ReadRecordAt(f, offset)
		if err == io.EOF {
			// Tail position: wait for new data.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-compactionCh:
				newF, openErr := os.Open(f.Name())
				if openErr != nil {
					return openErr
				}
				if f != s.walFile {
					f.Close()
				}
				f = newF
				offset = 0
				compactionCh = s.compactionNotify()
			case <-time.After(s.pollInterval):
				// retry
			}
			continue
		}
		if err != nil {
			return err
		}

		// Compute full record size on disk:
		// [Header:16][Payload:N][CRC32:4]
		recordSize := int64(wal.RecordHeaderSize) + int64(len(rec.Data)) + int64(wal.RecordChecksumSize)

		// Read raw bytes for transmission (preserves original CRC).
		raw := make([]byte, recordSize)
		if _, err := f.ReadAt(raw, offset); err != nil {
			return err
		}

		entry := RawEntry{
			Offset:     offset,
			RawData:    raw,
			RecordSize: recordSize,
		}

		if err := fn(entry); err != nil {
			return err
		}

		offset += recordSize
		s.lag.UpdatePrimary(offset)
	}
}

// readUint32At reads a 4-byte big-endian uint32 from f at offset.
// Helper used internally.
func readUint32At(f *os.File, offset int64) (uint32, error) {
	buf := make([]byte, 4)
	if _, err := f.ReadAt(buf, offset); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(buf), nil
}
```

- [ ] **Step 4: 테스트 통과 확인**

```bash
go test ./internal/infrastructure/replication/... -run TestStreamer_SendsRecords -v
```

Expected: PASS.

- [ ] **Step 5: 커밋**

```bash
git add internal/infrastructure/replication/streamer.go internal/infrastructure/replication/streamer_test.go
git commit -m "feat(replication): add Streamer for Primary WAL tail-follow with compaction awareness"
```

---

## Task 6: replication/receiver.go — Replica WAL append

**Files:**
- Create: `internal/infrastructure/replication/receiver.go`

- [ ] **Step 1: 테스트 작성**

Create `internal/infrastructure/replication/receiver_test.go`:

```go
package replication_test

import (
	"os"
	"testing"

	"github.com/junyoung/core-x/internal/infrastructure/replication"
	"github.com/junyoung/core-x/internal/infrastructure/storage/wal"
)

func TestReceiver_AppendAndRecover(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	// Write 2 records to source WAL.
	srcPath := srcDir + "/src.wal"
	sw, err := wal.NewWriter(wal.Config{Path: srcPath, SyncPolicy: wal.SyncNever})
	if err != nil {
		t.Fatal(err)
	}
	sw.Write([]byte("record-1"))
	sw.Write([]byte("record-2"))
	sw.Sync()
	sw.Close()

	// Read raw bytes from source WAL.
	srcFile, err := os.Open(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer srcFile.Close()

	var entries []replication.RawEntry
	offset := int64(0)
	for {
		rec, err := wal.ReadRecordAt(srcFile, offset)
		if err != nil {
			break
		}
		size := int64(wal.RecordHeaderSize) + int64(len(rec.Data)) + int64(wal.RecordChecksumSize)
		raw := make([]byte, size)
		srcFile.ReadAt(raw, offset)
		entries = append(entries, replication.RawEntry{Offset: offset, RawData: raw, RecordSize: size})
		offset += size
	}

	if len(entries) != 2 {
		t.Fatalf("expected 2 entries from source, got %d", len(entries))
	}

	// Write to destination WAL via Receiver.
	dstPath := dstDir + "/dst.wal"
	recv, err := replication.NewReceiver(dstPath)
	if err != nil {
		t.Fatal(err)
	}
	defer recv.Close()

	for _, e := range entries {
		if err := recv.Append(e); err != nil {
			t.Fatalf("Append failed: %v", err)
		}
	}

	// Verify destination WAL has identical content.
	dstReader, err := wal.NewReader(dstPath)
	if err != nil {
		t.Fatal(err)
	}
	defer dstReader.Close()

	count := 0
	for dstReader.Scan() {
		count++
	}
	if dstReader.Err() != nil {
		t.Fatalf("dst WAL scan error: %v", dstReader.Err())
	}
	if count != 2 {
		t.Errorf("dst WAL has %d records, want 2", count)
	}
}
```

- [ ] **Step 2: 테스트 실패 확인**

```bash
go test ./internal/infrastructure/replication/... -run TestReceiver_AppendAndRecover -v
```

Expected: FAIL — `replication.Receiver` undefined.

- [ ] **Step 3: receiver.go 구현**

Create `internal/infrastructure/replication/receiver.go`:

```go
package replication

import (
	"os"
	"sync"
)

// Receiver appends raw WAL entries received from a primary to a local WAL file.
// It is used on the replica side to persist replicated data.
//
// Receiver does NOT use wal.Writer because we bypass encoding — raw bytes are
// appended verbatim to preserve the primary's CRC32 checksums.
type Receiver struct {
	mu   sync.Mutex
	file *os.File
	path string
}

// NewReceiver opens (or creates) the replica WAL file at path for append-only writing.
func NewReceiver(path string) (*Receiver, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	return &Receiver{file: f, path: path}, nil
}

// Append writes raw WAL bytes to the replica WAL file and calls fsync.
// The raw bytes must be a complete WAL record (header + payload + checksum).
func (r *Receiver) Append(entry RawEntry) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, err := r.file.Write(entry.RawData); err != nil {
		return err
	}
	return r.file.Sync()
}

// PersistedOffset returns the current file size, which represents the last
// successfully fsync'd byte offset. Used as start_offset on reconnect.
func (r *Receiver) PersistedOffset() (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	info, err := r.file.Stat()
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// Close closes the replica WAL file.
func (r *Receiver) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.file != nil {
		return r.file.Close()
	}
	return nil
}
```

- [ ] **Step 4: 테스트 통과 확인**

```bash
go test ./internal/infrastructure/replication/... -run TestReceiver_AppendAndRecover -v
```

Expected: PASS.

- [ ] **Step 5: 커밋**

```bash
git add internal/infrastructure/replication/receiver.go internal/infrastructure/replication/receiver_test.go
git commit -m "feat(replication): add Receiver for replica-side raw WAL append with fsync"
```

---

## Task 7: replication/server.go — gRPC ReplicationService

**Files:**
- Create: `internal/infrastructure/replication/server.go`

- [ ] **Step 1: 테스트 작성**

Create `internal/infrastructure/replication/server_test.go`:

```go
package replication_test

import (
	"context"
	"net"
	"os"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/junyoung/core-x/internal/infrastructure/replication"
	"github.com/junyoung/core-x/internal/infrastructure/storage/wal"
	pb "github.com/junyoung/core-x/proto/pb"
)

func TestReplicationServer_StreamWAL(t *testing.T) {
	dir := t.TempDir()
	walPath := dir + "/test.wal"

	// Write 2 records.
	w, err := wal.NewWriter(wal.Config{Path: walPath, SyncPolicy: wal.SyncNever})
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("entry-A"))
	w.Write([]byte("entry-B"))
	w.Sync()
	w.Close()

	walFile, err := os.Open(walPath)
	if err != nil {
		t.Fatal(err)
	}
	defer walFile.Close()

	lag := replication.NewReplicationLag()
	streamer := replication.NewStreamer(walFile, w.CompactionNotify, lag, 10*time.Millisecond)

	// Start gRPC server.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer()
	pb.RegisterReplicationServiceServer(gs, replication.NewReplicationServer(streamer))
	go gs.Serve(lis)
	defer gs.Stop()

	// Connect as client.
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	client := pb.NewReplicationServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	stream, err := client.StreamWAL(ctx, &pb.StreamWALRequest{ReplicaId: "test-replica", StartOffset: 0})
	if err != nil {
		t.Fatal(err)
	}

	var received []*pb.WALEntry
	for {
		entry, err := stream.Recv()
		if err != nil {
			break
		}
		received = append(received, entry)
		if len(received) == 2 {
			cancel()
		}
	}

	if len(received) != 2 {
		t.Errorf("received %d entries, want 2", len(received))
	}
}
```

- [ ] **Step 2: 테스트 실패 확인**

```bash
go test ./internal/infrastructure/replication/... -run TestReplicationServer_StreamWAL -v
```

Expected: FAIL — `replication.ReplicationServer` undefined.

- [ ] **Step 3: server.go 구현**

Create `internal/infrastructure/replication/server.go`:

```go
package replication

import (
	"log/slog"

	pb "github.com/junyoung/core-x/proto/pb"
)

// ReplicationServer implements the gRPC ReplicationService.
// It wraps Streamer to push WAL entries to connecting replicas.
type ReplicationServer struct {
	pb.UnimplementedReplicationServiceServer
	streamer *Streamer
}

// NewReplicationServer creates a ReplicationServer backed by the given Streamer.
func NewReplicationServer(streamer *Streamer) *ReplicationServer {
	return &ReplicationServer{streamer: streamer}
}

// StreamWAL handles a replica's request to stream WAL entries.
// It opens a tail-following stream starting at req.StartOffset.
// The stream ends when the client disconnects (ctx is cancelled).
func (s *ReplicationServer) StreamWAL(req *pb.StreamWALRequest, stream pb.ReplicationService_StreamWALServer) error {
	slog.Info("replication: replica connected",
		"replica_id", req.ReplicaId,
		"start_offset", req.StartOffset,
	)

	err := s.streamer.Stream(stream.Context(), req.StartOffset, func(entry RawEntry) error {
		return stream.Send(&pb.WALEntry{
			Offset:     entry.Offset,
			RawData:    entry.RawData,
			RecordSize: entry.RecordSize,
		})
	})

	if err != nil && stream.Context().Err() == nil {
		slog.Warn("replication: streamer error", "replica_id", req.ReplicaId, "err", err)
	}

	slog.Info("replication: replica disconnected", "replica_id", req.ReplicaId)
	return err
}
```

- [ ] **Step 4: 테스트 통과 확인**

```bash
go test ./internal/infrastructure/replication/... -run TestReplicationServer_StreamWAL -v
```

Expected: PASS.

- [ ] **Step 5: 커밋**

```bash
git add internal/infrastructure/replication/server.go internal/infrastructure/replication/server_test.go
git commit -m "feat(replication): add gRPC ReplicationServer delegating to Streamer"
```

---

## Task 8: replication/client.go — Replica reconnect loop

**Files:**
- Create: `internal/infrastructure/replication/client.go`

- [ ] **Step 1: 테스트 작성**

Create `internal/infrastructure/replication/client_test.go`:

```go
package replication_test

import (
	"context"
	"net"
	"os"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/junyoung/core-x/internal/infrastructure/replication"
	"github.com/junyoung/core-x/internal/infrastructure/storage/wal"
	pb "github.com/junyoung/core-x/proto/pb"
)

func TestReplicationClient_ReceivesEntries(t *testing.T) {
	dir := t.TempDir()
	walPath := dir + "/primary.wal"
	replicaPath := dir + "/replica.wal"

	// Write 3 records to primary WAL.
	pw, err := wal.NewWriter(wal.Config{Path: walPath, SyncPolicy: wal.SyncNever})
	if err != nil {
		t.Fatal(err)
	}
	pw.Write([]byte("alpha"))
	pw.Write([]byte("beta"))
	pw.Write([]byte("gamma"))
	pw.Sync()
	pw.Close()

	// Start gRPC replication server.
	walFile, err := os.Open(walPath)
	if err != nil {
		t.Fatal(err)
	}
	defer walFile.Close()

	lag := replication.NewReplicationLag()
	streamer := replication.NewStreamer(walFile, pw.CompactionNotify, lag, 10*time.Millisecond)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer()
	pb.RegisterReplicationServiceServer(gs, replication.NewReplicationServer(streamer))
	go gs.Serve(lis)
	defer gs.Stop()

	// Start replica receiver.
	receiver, err := replication.NewReceiver(replicaPath)
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()

	// Start replication client.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	client := replication.NewReplicationClient(
		lis.Addr().String(),
		"replica-test",
		receiver,
		lag,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)

	done := make(chan error, 1)
	go func() {
		done <- client.Run(ctx)
	}()

	// Wait until replica WAL has 3 records.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		offset, _ := receiver.PersistedOffset()
		if offset > 0 {
			// Check record count via reader.
			r, err := wal.NewReader(replicaPath)
			if err == nil {
				count := 0
				for r.Scan() {
					count++
				}
				r.Close()
				if count == 3 {
					cancel()
					break
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}

	<-done

	// Final verification.
	r, err := wal.NewReader(replicaPath)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	count := 0
	for r.Scan() {
		count++
	}
	if count != 3 {
		t.Errorf("replica WAL has %d records, want 3", count)
	}
}
```

- [ ] **Step 2: 테스트 실패 확인**

```bash
go test ./internal/infrastructure/replication/... -run TestReplicationClient_ReceivesEntries -v -timeout 15s
```

Expected: FAIL — `replication.ReplicationClient` undefined.

- [ ] **Step 3: client.go 구현**

Create `internal/infrastructure/replication/client.go`:

```go
package replication

import (
	"context"
	"io"
	"log/slog"
	"time"

	"google.golang.org/grpc"

	pb "github.com/junyoung/core-x/proto/pb"
)

// ReplicationClient connects to a primary's ReplicationService and feeds
// received WAL entries into a Receiver. It reconnects automatically on
// disconnection using exponential backoff.
type ReplicationClient struct {
	primaryAddr string
	replicaID   string
	receiver    *Receiver
	lag         *ReplicationLag
	dialOpts    []grpc.DialOption
}

// NewReplicationClient creates a new ReplicationClient.
// dialOpts are passed to grpc.NewClient (e.g. TLS credentials).
func NewReplicationClient(primaryAddr, replicaID string, receiver *Receiver, lag *ReplicationLag, dialOpts ...grpc.DialOption) *ReplicationClient {
	return &ReplicationClient{
		primaryAddr: primaryAddr,
		replicaID:   replicaID,
		receiver:    receiver,
		lag:         lag,
		dialOpts:    dialOpts,
	}
}

// Run starts the replication loop. It connects to the primary, streams WAL entries,
// and reconnects on failure. Returns when ctx is cancelled.
func (c *ReplicationClient) Run(ctx context.Context) error {
	backoff := 100 * time.Millisecond

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		if err := c.runOnce(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			slog.Warn("replication: client error, reconnecting",
				"primary", c.primaryAddr,
				"err", err,
				"backoff", backoff,
			)
		}

		c.lag.IncReconnect()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}

		// Exponential backoff, capped at 10s.
		backoff *= 2
		if backoff > 10*time.Second {
			backoff = 10 * time.Second
		}
	}
}

// runOnce performs a single connection attempt and streams until error or ctx done.
func (c *ReplicationClient) runOnce(ctx context.Context) error {
	conn, err := grpc.NewClient(c.primaryAddr, c.dialOpts...)
	if err != nil {
		return err
	}
	defer conn.Close()

	client := pb.NewReplicationServiceClient(conn)

	// start_offset = last persisted byte offset on this replica.
	startOffset, err := c.receiver.PersistedOffset()
	if err != nil {
		return err
	}
	c.lag.UpdateReplica(startOffset)

	stream, err := client.StreamWAL(ctx, &pb.StreamWALRequest{
		ReplicaId:   c.replicaID,
		StartOffset: startOffset,
	})
	if err != nil {
		return err
	}

	slog.Info("replication: connected to primary",
		"primary", c.primaryAddr,
		"start_offset", startOffset,
	)

	// Reset backoff on successful connection is handled by caller resetting backoff
	// when runOnce returns nil — but since any clean context cancellation returns nil
	// via ctx.Err(), we reset backoff implicitly. This is the intended behavior.

	for {
		entry, err := stream.Recv()
		if err == io.EOF || ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return err
		}

		if appErr := c.receiver.Append(RawEntry{
			Offset:     entry.Offset,
			RawData:    entry.RawData,
			RecordSize: entry.RecordSize,
		}); appErr != nil {
			return appErr
		}

		c.lag.UpdateReplica(entry.Offset + entry.RecordSize)
	}
}
```

- [ ] **Step 4: 테스트 통과 확인**

```bash
go test ./internal/infrastructure/replication/... -run TestReplicationClient_ReceivesEntries -v -timeout 15s
```

Expected: PASS.

- [ ] **Step 5: 전체 replication 패키지 테스트**

```bash
go test ./internal/infrastructure/replication/... -v -timeout 30s
```

Expected: 전체 PASS.

- [ ] **Step 6: 커밋**

```bash
git add internal/infrastructure/replication/client.go internal/infrastructure/replication/client_test.go
git commit -m "feat(replication): add ReplicationClient with exponential backoff reconnect loop"
```

---

## Task 9: cmd/main.go — Wiring

**Files:**
- Modify: `cmd/main.go`

- [ ] **Step 1: 빌드 확인 (수정 전)**

```bash
go build ./...
```

Expected: 빌드 성공.

- [ ] **Step 2: main.go 수정 — replication wiring 추가**

`cmd/main.go`에서:

1. import 블록에 추가:

```go
"github.com/junyoung/core-x/internal/infrastructure/cluster"
infrareplication "github.com/junyoung/core-x/internal/infrastructure/replication"
```

2. 설정 섹션에 추가 (기존 `forwardTimeout` 파싱 직후):

```go
// Phase 3b: Replication 설정.
role, err := cluster.RoleFromEnv()
if err != nil {
    slog.Error("invalid CORE_X_ROLE", "err", err)
    os.Exit(1)
}
primaryAddr := envOr("CORE_X_PRIMARY_ADDR", "")
replicaWALPath := envOr("CORE_X_REPLICA_WAL_PATH", "./data/replica.wal")
```

3. `--- Phase 3: 클러스터 초기화 ---` 블록 직후, `--- HTTP 라우터 ---` 직전에 추가:

```go
// --- Phase 3b: Replication 초기화 ------------------------------------------
var replLag *infrareplication.ReplicationLag

if role == cluster.RolePrimary {
    // Primary: WAL 파일을 열고 Streamer를 gRPC 서버에 등록.
    replLag = infrareplication.NewReplicationLag()

    walReadFile, err := os.Open(walPath)
    if err != nil {
        slog.Error("replication: failed to open wal for streaming", "path", walPath, "err", err)
        os.Exit(1)
    }
    // walReadFile은 프로세스 종료 시 OS가 자동으로 닫음 (graceful shutdown에서 명시적으로 닫아도 됨).

    streamer := infrareplication.NewStreamer(walReadFile, walWriter.CompactionNotify, replLag, 10*time.Millisecond)
    replServer := infrareplication.NewReplicationServer(streamer)

    // gRPC 서버에 ReplicationService 등록.
    // grpcSrv는 이미 위에서 생성된 *infragrpc.Server.
    // RegisterReplicationService를 위해 내부 grpc.Server를 노출해야 한다.
    // → infragrpc.Server에 RegisterService 메서드 추가가 필요하거나,
    //   ReplicationService를 별도 gRPC 포트로 노출한다.
    //
    // 설계 선택: 별도 포트 없이 기존 gRPC 서버를 재사용.
    // infragrpc.Server에 RegisterReplicationService() 메서드 추가.
    if grpcSrv != nil {
        grpcSrv.RegisterReplicationService(replServer)
    }

    slog.Info("replication: primary mode enabled", "wal_path", walPath)

} else if role == cluster.RoleReplica {
    // Replica: Receiver를 생성하고 Client를 백그라운드에서 실행.
    replLag = infrareplication.NewReplicationLag()

    replicaDir := filepath.Dir(replicaWALPath)
    if err := os.MkdirAll(replicaDir, 0750); err != nil {
        slog.Error("replication: failed to create replica wal dir", "err", err)
        os.Exit(1)
    }

    receiver, err := infrareplication.NewReceiver(replicaWALPath)
    if err != nil {
        slog.Error("replication: failed to open replica wal", "path", replicaWALPath, "err", err)
        os.Exit(1)
    }

    replClient := infrareplication.NewReplicationClient(
        primaryAddr,
        nodeID,
        receiver,
        replLag,
        grpc.WithTransportCredentials(insecure.NewCredentials()),
    )

    replCtx, replCancel := context.WithCancel(context.Background())
    defer replCancel()

    go func() {
        if err := replClient.Run(replCtx); err != nil && replCtx.Err() == nil {
            slog.Error("replication client stopped unexpectedly", "err", err)
        }
    }()

    slog.Info("replication: replica mode enabled",
        "primary_addr", primaryAddr,
        "replica_wal_path", replicaWALPath,
    )
}
```

4. `/stats` 핸들러를 확장하여 `replication_lag_bytes`를 포함:

`mux.HandleFunc("GET /stats", infrahttp.StatsHandler(workerPool))` 줄을:

```go
mux.HandleFunc("GET /stats", infrahttp.StatsHandler(workerPool, replLag))
```

로 변경. `infrahttp.StatsHandler`가 `*infrareplication.ReplicationLag`를 두 번째 인자로 받도록 수정 필요 (아래 Task 10).

- [ ] **Step 3: infragrpc.Server에 RegisterReplicationService 추가**

`internal/infrastructure/grpc/server.go` 수정 — `Serve()` 메서드 아래에 추가:

```go
// RegisterReplicationService registers a ReplicationService implementation
// with the underlying gRPC server. Must be called before Serve().
func (s *Server) RegisterReplicationService(srv pb.ReplicationServiceServer) {
	pb.RegisterReplicationServiceServer(s.grpcServer, srv)
}
```

- [ ] **Step 4: 빌드 확인**

```bash
go build ./...
```

Expected: 빌드 성공.

- [ ] **Step 5: 커밋**

```bash
git add cmd/main.go internal/infrastructure/grpc/server.go
git commit -m "feat(main): wire replication components for Phase 3b primary/replica mode"
```

---

## Task 10: infrahttp.StatsHandler — replication_lag_bytes 노출

**Files:**
- Modify: `internal/infrastructure/http/stats.go`

- [ ] **Step 1: stats.go 읽기**

```bash
cat internal/infrastructure/http/stats.go
```

- [ ] **Step 2: StatsHandler 시그니처 및 응답에 replication_lag_bytes 추가**

`stats.go`에서 `StatsHandler` 함수 시그니처를 변경하여 옵셔널 lag 파라미터 수용:

```go
// 기존:
func StatsHandler(stats executor.Stats) http.HandlerFunc

// 변경 후:
func StatsHandler(stats executor.Stats, lag ...*replication.ReplicationLag) http.HandlerFunc
```

응답 JSON에 추가:

```go
type statsResponse struct {
    // ... 기존 필드 ...
    ReplicationLagBytes    int64 `json:"replication_lag_bytes,omitempty"`
    ReplicationReconnects  int64 `json:"replication_reconnects,omitempty"`
}

// lag가 있으면 채움:
if len(lag) > 0 && lag[0] != nil {
    resp.ReplicationLagBytes = lag[0].Bytes()
    resp.ReplicationReconnects = lag[0].ReconnectCount()
}
```

- [ ] **Step 3: import 추가**

`stats.go`에 import 추가:

```go
"github.com/junyoung/core-x/internal/infrastructure/replication"
```

- [ ] **Step 4: 빌드 및 전체 테스트**

```bash
go build ./...
go test ./... -timeout 60s
```

Expected: 빌드 성공, 전체 테스트 PASS.

- [ ] **Step 5: README.md Phase 3 섹션 업데이트**

`README.md`의 Phase 3 roadmap 섹션:

```markdown
### Phase 3: Distributed Scalability ✅ COMPLETE
- [x] Node-to-Node Communication (gRPC) — `infrastructure/grpc/`
- [x] Consistent Hashing for Partitioning — Virtual Nodes, `infrastructure/cluster/`
- [x] Basic Replication — Primary→Replica async WAL streaming — `infrastructure/replication/`
```

ADR 링크 추가:

```markdown
- [ADR-008: Basic Replication — Async WAL Streaming](docs/adr/0008-basic-replication-async-wal-streaming.md)
```

- [ ] **Step 6: 최종 커밋**

```bash
git add internal/infrastructure/http/stats.go README.md
git commit -m "feat(http): expose replication_lag_bytes in /stats; mark Phase 3 complete"
```

---

## Self-Review

**Spec coverage 체크:**
- [x] Primary → Replica async WAL streaming (Task 5, 7)
- [x] Replica crash recovery via reconnect + PersistedOffset (Task 6, 8)
- [x] Compaction 중 Replication 처리 (Task 3, 5)
- [x] Proto 확장 (Task 1)
- [x] 역할 결정 (Task 2)
- [x] Lag 모니터링 (Task 4, 10)
- [x] cmd/main.go wiring (Task 9)

**Type consistency:**
- `RawEntry` — Task 5에서 정의, Task 6, 7, 8에서 동일하게 사용
- `ReplicationLag` — Task 4에서 정의, Task 5, 7, 8, 9, 10에서 동일하게 사용
- `Receiver.Append(RawEntry)` — Task 6에서 정의, Task 8에서 동일하게 호출
- `NewStreamer(walFile, compactionNotify func() <-chan struct{}, lag, pollInterval)` — Task 5에서 정의, Task 7, 9에서 동일하게 호출

**주의사항:**
- Task 9에서 grpc.WithTransportCredentials import가 필요 → `google.golang.org/grpc/credentials/insecure`
- Task 9의 `replLag`가 nil일 수 있으므로 Task 10의 StatsHandler는 nil 체크 포함
- `w.CompactionNotify` (Task 5)에서 `w`가 Close 후에도 채널이 유효한지 확인 필요 — Close는 채널을 닫지 않으므로 안전
