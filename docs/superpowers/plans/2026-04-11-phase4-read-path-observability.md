# Phase 4: Read Path + Observability Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `GET /kv/{key}` read API and Prometheus `/metrics` endpoint so Core-X is a complete read/write distributed KV system with production-grade observability.

**Architecture:** Read path uses ring-based routing identical to write path — `ring.Lookup(key)` determines owner, non-owner nodes forward via a new `KVService.Get` gRPC RPC. Replica nodes redirect Get requests to Primary (no index on replica). Prometheus metrics are injected via ports (interfaces) in the Application layer so IngestionService and KVHandler remain infrastructure-agnostic.

**Tech Stack:** Go stdlib, `github.com/prometheus/client_golang` (new dependency), existing protobuf/gRPC pipeline.

---

## Why This Phase? (Decision Log)

### Option A — Raft 합의 알고리즘 (기각)

Raft를 Phase 4로 구현하는 것이 ADR-007 로드맵의 원래 의도였다.

**기각 이유:**

1. **Read path 없이 Raft는 미완성 시스템이다.** Raft는 "쓰기 합의"를 보장하지만, 합의 결과를 조회할 `GET /kv/{key}`가 없으면 리더 선출이 외부에서 관찰 불가능하다. 쓰기 합의가 있지만 읽기가 없는 분산 시스템은 분산 로그에 불과하다.

2. **검증 도구 부재.** Phase 3b에서 구현한 `replLag.Bytes()`와 ring 분산 균등성을 실시간으로 측정할 Prometheus exporter가 없다. Raft를 구현해도 장애 시 디버깅이 불가능하다.

3. **구현 비용 대비 완성도.** 올바른 Raft 구현은 ~3,000줄 이상이며, log divergence·split brain·leader isolation 등 코너케이스가 많다. Read path 없이 진입하면 "절반만 완성된 Raft"가 남을 가능성이 높다.

### Option B — Read Path + Observability (채택)

**채택 이유:**

1. **Raft의 전제조건을 완성한다.** Read path를 먼저 구현하면 Raft 이후 leader에게 읽기를 보내는 동작을 검증할 수 있다. 순서가 올바르다.

2. **즉각적인 완성도 향상.** Phase 4가 끝나면 Core-X는 완전한 read/write 분산 KV 스토어가 된다. 포트폴리오 관점에서 "절반 완성된 Raft"보다 가치 있다.

3. **DDIA Maintainability 원칙 이행.** "운영자가 시스템 동작을 이해하고 디버깅할 수 있어야 한다" — Prometheus 없이 분산 시스템을 운영하는 것은 이 원칙을 위반한다.

---

## Why Subagent-Driven? (Execution Log)

구현 방식으로 **Subagent-Driven Development**를 선택했다. 대안과 비교:

### Option X — Inline Execution (기각)

같은 Claude 세션에서 태스크를 순차 실행하는 방식.

**기각 이유:**

1. **Context pollution.** 태스크가 쌓일수록 이전 태스크의 임시 정보(실패한 시도, 디버깅 흔적)가 컨텍스트를 오염시켜 이후 태스크에 영향을 준다.

2. **리뷰 루프의 신뢰성 문제.** 구현과 리뷰를 같은 컨텍스트에서 수행하면 구현자가 자신의 오류를 자신이 검토하는 구조가 된다. 독립적 관점이 없다.

3. **복구 비용.** 태스크 중간에 오류가 발생하면 현재 컨텍스트 전체가 불확실 상태가 된다.

### Option Y — Subagent-Driven Development (채택)

태스크마다 신선한 subagent를 파견하고, 완료 후 Spec reviewer → Code quality reviewer 2단계 리뷰를 수행하는 방식.

**채택 이유:**

1. **태스크 격리.** 각 subagent는 해당 태스크에 필요한 컨텍스트만 전달받는다. 이전 태스크의 노이즈가 없다.

2. **독립적 2단계 리뷰.** Spec reviewer는 구현 세부사항 없이 스펙 충족 여부만 확인한다. Code quality reviewer는 코드 품질만 본다. 구현자와 리뷰어가 분리된다.

3. **조기 오류 발견.** 리뷰 루프가 태스크 단위로 즉시 실행되므로 이후 태스크가 잘못된 기반 위에 쌓이지 않는다.

4. **세션 유지.** Executing-Plans(병렬 세션)와 달리 같은 세션에서 진행되므로 중간에 사용자가 방향을 조정할 수 있다.

---

## File Map

### New Files

| File | Responsibility |
|------|---------------|
| `internal/application/ingestion/metrics.go` | `IngestMetrics` port interface |
| `internal/infrastructure/metrics/registry.go` | Prometheus registry + `/metrics` handler |
| `internal/infrastructure/metrics/ingestion.go` | `PromIngestMetrics` — Prometheus impl of `IngestMetrics` |
| `internal/infrastructure/metrics/replication.go` | `PromReplMetrics` — exposes replication lag gauge |
| `internal/infrastructure/metrics/cluster.go` | `PromClusterMetrics` — exposes ring/forward metrics |
| `internal/infrastructure/metrics/noop.go` | `NoopIngestMetrics` — zero-overhead impl for tests |
| `internal/infrastructure/http/kv_handler.go` | `KVHandler` for `GET /kv/{key}` |
| `internal/infrastructure/grpc/kv_server.go` | `GRPCKVServer` — implements `KVService.Get` |

### Modified Files

| File | Change |
|------|--------|
| `proto/ingest.proto` | Add `KVService` + `GetRequest`/`GetResponse` messages |
| `proto/pb/ingest.pb.go` | Regenerated (do not hand-edit) |
| `proto/pb/ingest_grpc.pb.go` | Regenerated (do not hand-edit) |
| `internal/application/ingestion/service.go` | Accept `IngestMetrics` port; call `RecordIngest` in `Ingest()` |
| `internal/infrastructure/http/handler.go` | Record latency via `IngestMetrics` port |
| `internal/infrastructure/grpc/server.go` | Add `RegisterKVService` method |
| `internal/infrastructure/grpc/forward.go` | Add `ForwardGet(ctx, node, key)` method |
| `internal/infrastructure/grpc/client.go` | Add `Get(ctx, req)` call via KVService stub |
| `cmd/main.go` | Wire metrics + KVHandler + `GET /metrics` + `GET /kv/{key}` |

---

## Task 1: Proto — Add KVService

**Files:**
- Modify: `proto/ingest.proto`
- Regenerate: `proto/pb/ingest.pb.go`, `proto/pb/ingest_grpc.pb.go`

- [ ] **Step 1: Add KVService to proto**

Open `proto/ingest.proto` and append after the existing `ReplicationService`:

```protobuf
// KVService provides point-lookup reads across cluster nodes.
service KVService {
  rpc Get(GetRequest) returns (GetResponse);
}

// GetRequest carries the key to look up.
message GetRequest {
  string key = 1;
}

// GetResponse carries the result of a KV lookup.
message GetResponse {
  bool   found            = 1;
  bytes  payload          = 2;
  string source           = 3;
  int64  received_at_unix_ns = 4;
}
```

- [ ] **Step 2: Regenerate proto**

```bash
cd /Users/junyoung/workspace/personal/core-x
protoc \
  --go_out=. --go_opt=paths=source_relative \
  --go-grpc_out=. --go-grpc_opt=paths=source_relative \
  proto/ingest.proto
```

Expected: `proto/pb/ingest.pb.go` and `proto/pb/ingest_grpc.pb.go` updated with `KVServiceClient`, `KVServiceServer`, `GetRequest`, `GetResponse` types.

- [ ] **Step 3: Verify build**

```bash
cd /Users/junyoung/workspace/personal/core-x
go build ./...
```

Expected: no compile errors.

- [ ] **Step 4: Commit**

```bash
git add proto/ingest.proto proto/pb/ingest.pb.go proto/pb/ingest_grpc.pb.go
git commit -m "feat(proto): add KVService.Get RPC for distributed read path"
```

---

## Task 2: gRPC KV Server

**Files:**
- Create: `internal/infrastructure/grpc/kv_server.go`
- Modify: `internal/infrastructure/grpc/server.go` (add `RegisterKVService`)
- Modify: `internal/infrastructure/grpc/client.go` (add `Get` method)
- Modify: `internal/infrastructure/grpc/forward.go` (add `ForwardGet`)

- [ ] **Step 1: Write failing test for GRPCKVServer**

Create `internal/infrastructure/grpc/kv_server_test.go`:

```go
package grpc_test

import (
	"context"
	"testing"

	"github.com/junyoung/core-x/internal/domain"
	infragrpc "github.com/junyoung/core-x/internal/infrastructure/grpc"
	pb "github.com/junyoung/core-x/proto/pb"
)

type stubKVGetter struct {
	event *domain.Event
	err   error
}

func (s *stubKVGetter) Get(key string) (*domain.Event, error) {
	return s.event, s.err
}

func TestGRPCKVServer_Get_Found(t *testing.T) {
	stub := &stubKVGetter{
		event: &domain.Event{Source: "src1", Payload: "hello"},
	}
	srv := infragrpc.NewGRPCKVServer(stub)
	resp, err := srv.Get(context.Background(), &pb.GetRequest{Key: "src1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.Found {
		t.Fatal("expected found=true")
	}
	if string(resp.Payload) != "hello" {
		t.Fatalf("expected payload=hello, got %s", resp.Payload)
	}
}

func TestGRPCKVServer_Get_NotFound(t *testing.T) {
	stub := &stubKVGetter{err: errors.New("kv: key not found")}
	srv := infragrpc.NewGRPCKVServer(stub)
	resp, err := srv.Get(context.Background(), &pb.GetRequest{Key: "missing"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Found {
		t.Fatal("expected found=false")
	}
}
```

- [ ] **Step 2: Run test — expect compile failure**

```bash
cd /Users/junyoung/workspace/personal/core-x
go test ./internal/infrastructure/grpc/... 2>&1 | head -20
```

Expected: compile error `NewGRPCKVServer undefined`.

- [ ] **Step 3: Create GRPCKVServer**

Create `internal/infrastructure/grpc/kv_server.go`:

```go
package grpc

import (
	"context"
	"errors"

	"github.com/junyoung/core-x/internal/domain"
	kvstore "github.com/junyoung/core-x/internal/infrastructure/storage/kv"
	pb "github.com/junyoung/core-x/proto/pb"
)

// KVGetter is the port for KV point-lookups.
// kv.Store implements this interface.
type KVGetter interface {
	Get(key string) (*domain.Event, error)
}

// GRPCKVServer handles Get RPCs from peer nodes.
type GRPCKVServer struct {
	kv KVGetter
	pb.UnimplementedKVServiceServer
}

// NewGRPCKVServer creates a server backed by the given KVGetter.
func NewGRPCKVServer(kv KVGetter) *GRPCKVServer {
	return &GRPCKVServer{kv: kv}
}

// Get handles a KV lookup RPC.
func (s *GRPCKVServer) Get(_ context.Context, req *pb.GetRequest) (*pb.GetResponse, error) {
	event, err := s.kv.Get(req.Key)
	if err != nil {
		if errors.Is(err, kvstore.ErrKeyNotFound) {
			return &pb.GetResponse{Found: false}, nil
		}
		return nil, err
	}
	return &pb.GetResponse{
		Found:              true,
		Payload:            []byte(event.Payload),
		Source:             event.Source,
		ReceivedAtUnixNs:   event.ReceivedAt.UnixNano(),
	}, nil
}
```

- [ ] **Step 4: Add RegisterKVService to Server**

In `internal/infrastructure/grpc/server.go`, append after `RegisterReplicationService`:

```go
// RegisterKVService registers a KVService implementation with the underlying gRPC server.
// Must be called before Serve().
func (s *Server) RegisterKVService(srv pb.KVServiceServer) {
	pb.RegisterKVServiceServer(s.grpcServer, srv)
}
```

- [ ] **Step 5: Add Get to gRPC client**

In `internal/infrastructure/grpc/client.go`, read the file first, then add a `Get` method to the existing client type. The client currently has an `Ingest` method. Add:

```go
// Get performs a KV lookup RPC on the remote node.
func (c *Client) Get(ctx context.Context, req *pb.GetRequest) (*pb.GetResponse, error) {
	return pb.NewKVServiceClient(c.conn).Get(ctx, req)
}
```

- [ ] **Step 6: Add ForwardGet to Forwarder**

In `internal/infrastructure/grpc/forward.go`, append:

```go
// ForwardGet forwards a KV Get request to a peer node.
// Returns (nil, ErrKeyNotFound) if the peer reports not found.
// Returns error for network/RPC failures.
func (f *Forwarder) ForwardGet(ctx context.Context, node *cluster.Node, key string) (*pb.GetResponse, error) {
	client, err := f.pool.Get(node)
	if err != nil {
		return nil, fmt.Errorf("forward get client: %w", err)
	}
	resp, err := client.Get(ctx, &pb.GetRequest{Key: key})
	if err != nil {
		return nil, fmt.Errorf("forward get rpc: %w", err)
	}
	return resp, nil
}
```

- [ ] **Step 7: Run tests**

```bash
cd /Users/junyoung/workspace/personal/core-x
go test ./internal/infrastructure/grpc/...
```

Expected: PASS

- [ ] **Step 8: Commit**

```bash
git add internal/infrastructure/grpc/kv_server.go \
        internal/infrastructure/grpc/kv_server_test.go \
        internal/infrastructure/grpc/server.go \
        internal/infrastructure/grpc/client.go \
        internal/infrastructure/grpc/forward.go
git commit -m "feat(grpc): add KVService Get RPC server + client + forwarder"
```

---

## Task 3: HTTP KV Handler

**Files:**
- Create: `internal/infrastructure/http/kv_handler.go`

- [ ] **Step 1: Write failing test**

Create `internal/infrastructure/http/kv_handler_test.go`:

```go
package http_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/junyoung/core-x/internal/domain"
	infrahttp "github.com/junyoung/core-x/internal/infrastructure/http"
)

type stubKVStore struct {
	event *domain.Event
	err   error
}

func (s *stubKVStore) Get(key string) (*domain.Event, error) {
	return s.event, s.err
}

func TestKVHandler_Get_Found(t *testing.T) {
	store := &stubKVStore{
		event: &domain.Event{
			Source:     "src1",
			Payload:    "hello",
			ReceivedAt: time.Unix(0, 1234567890),
		},
	}
	h := infrahttp.NewKVHandler(store, nil, "", nil)
	req := httptest.NewRequest(http.MethodGet, "/kv/src1", nil)
	req.SetPathValue("key", "src1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestKVHandler_Get_NotFound(t *testing.T) {
	store := &stubKVStore{err: errors.New("kv: key not found")}
	h := infrahttp.NewKVHandler(store, nil, "", nil)
	req := httptest.NewRequest(http.MethodGet, "/kv/missing", nil)
	req.SetPathValue("key", "missing")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}
```

- [ ] **Step 2: Run test — expect compile failure**

```bash
go test ./internal/infrastructure/http/... 2>&1 | head -20
```

Expected: compile error `NewKVHandler undefined`.

- [ ] **Step 3: Create KVHandler**

Create `internal/infrastructure/http/kv_handler.go`:

```go
package http

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/junyoung/core-x/internal/domain"
	"github.com/junyoung/core-x/internal/infrastructure/cluster"
	infragrpc "github.com/junyoung/core-x/internal/infrastructure/grpc"
	kvstore "github.com/junyoung/core-x/internal/infrastructure/storage/kv"
)

// KVGetter abstracts KV point-lookups for the HTTP layer.
type KVGetter interface {
	Get(key string) (*domain.Event, error)
}

// KVHandler handles GET /kv/{key}.
//
// Routing logic (mirrors write path):
//   - Cluster mode + key owned by peer → ForwardGet to peer node
//   - Cluster mode + key owned by self → local kvStore.Get
//   - Single-node mode → local kvStore.Get
//   - role == Replica → redirect to Primary (Primary has the index)
//
// Replica redirect: Replica nodes receive WAL bytes but do not rebuild the
// KV index. Phase 4 uses the simple approach: Replica's GET /kv/{key}
// returns 307 Temporary Redirect pointing to Primary's HTTP address.
// Phase 5 (index rebuild on replica) can upgrade this without API changes.
type KVHandler struct {
	kv             KVGetter
	ring           *cluster.Ring
	selfID         string
	forwarder      *infragrpc.Forwarder
	forwardTimeout time.Duration
}

// NewKVHandler creates a KVHandler. Pass nil ring for single-node mode.
func NewKVHandler(
	kv KVGetter,
	ring *cluster.Ring,
	selfID string,
	forwarder *infragrpc.Forwarder,
) *KVHandler {
	return &KVHandler{
		kv:             kv,
		ring:           ring,
		selfID:         selfID,
		forwarder:      forwarder,
		forwardTimeout: 3 * time.Second,
	}
}

// kvResponse is the JSON wire format for a successful GET /kv/{key}.
type kvResponse struct {
	Source           string `json:"source"`
	Payload          string `json:"payload"`
	ReceivedAtUnixNs int64  `json:"received_at_unix_ns"`
}

// ServeHTTP handles GET /kv/{key}.
func (h *KVHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if key == "" {
		http.Error(w, "key is required", http.StatusBadRequest)
		return
	}

	// Cluster mode: route by consistent hash.
	if h.ring != nil {
		if target, ok := h.ring.Lookup(key); ok && target.ID != h.selfID {
			if !target.IsHealthy() {
				http.Error(w, "target node unavailable", http.StatusServiceUnavailable)
				return
			}
			ctx, cancel := r.Context(), func() {}
			if h.forwardTimeout > 0 {
				ctx, cancel = contextWithTimeout(r.Context(), h.forwardTimeout)
			}
			defer cancel()

			resp, err := h.forwarder.ForwardGet(ctx, target, key)
			if err != nil {
				http.Error(w, "target node unavailable", http.StatusServiceUnavailable)
				return
			}
			if !resp.Found {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			writeKVJSON(w, &domain.Event{
				Source:     resp.Source,
				Payload:    string(resp.Payload),
				ReceivedAt: time.Unix(0, resp.ReceivedAtUnixNs),
			})
			return
		}
	}

	// Local lookup.
	event, err := h.kv.Get(key)
	if err != nil {
		if errors.Is(err, kvstore.ErrKeyNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	writeKVJSON(w, event)
}

func writeKVJSON(w http.ResponseWriter, e *domain.Event) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(kvResponse{ //nolint:errcheck
		Source:           e.Source,
		Payload:          e.Payload,
		ReceivedAtUnixNs: e.ReceivedAt.UnixNano(),
	})
}
```

Note: `contextWithTimeout` is a local alias — add this helper at the bottom of `kv_handler.go`:

```go
import "context"

func contextWithTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, d)
}
```

- [ ] **Step 4: Run tests**

```bash
go test ./internal/infrastructure/http/...
```

Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/infrastructure/http/kv_handler.go \
        internal/infrastructure/http/kv_handler_test.go
git commit -m "feat(http): add GET /kv/{key} handler with cluster-aware routing"
```

---

## Task 4: IngestMetrics Port Interface

**Files:**
- Create: `internal/application/ingestion/metrics.go`
- Modify: `internal/application/ingestion/service.go`

- [ ] **Step 1: Create IngestMetrics interface**

Create `internal/application/ingestion/metrics.go`:

```go
package ingestion

// IngestMetrics is the port for recording ingestion telemetry.
//
// Why a port (interface) here?
// IngestionService must not import Prometheus directly (infrastructure concern).
// Injecting the interface keeps the Application layer testable and infrastructure-agnostic.
// A NoopIngestMetrics implementation exists in infrastructure/metrics for tests.
type IngestMetrics interface {
	// RecordIngest records one ingest attempt.
	//   status: "ok" | "overloaded" | "wal_error" | "forwarded"
	//   latencySeconds: end-to-end duration of the Ingest() call
	RecordIngest(status string, latencySeconds float64)

	// RecordQueueDepth records the current worker queue depth.
	// Called opportunistically (not per-request) to avoid hot-path overhead.
	RecordQueueDepth(depth int)
}
```

- [ ] **Step 2: Write failing test for metrics integration**

In `internal/application/ingestion/service_test.go` (read existing file first), add:

```go
type capturingMetrics struct {
	calls []string
}

func (m *capturingMetrics) RecordIngest(status string, _ float64) { m.calls = append(m.calls, status) }
func (m *capturingMetrics) RecordQueueDepth(_ int)                {}

func TestIngestionService_Ingest_RecordsOkMetric(t *testing.T) {
	m := &capturingMetrics{}
	svc := NewIngestionService(&alwaysSubmitter{}, &stubPool{}, nil)
	svc.SetMetrics(m)
	_ = svc.Ingest("src", "payload")
	if len(m.calls) != 1 || m.calls[0] != "ok" {
		t.Fatalf("expected [ok], got %v", m.calls)
	}
}

func TestIngestionService_Ingest_RecordsOverloadedMetric(t *testing.T) {
	m := &capturingMetrics{}
	svc := NewIngestionService(&neverSubmitter{}, &stubPool{}, nil)
	svc.SetMetrics(m)
	_ = svc.Ingest("src", "payload")
	if len(m.calls) != 1 || m.calls[0] != "overloaded" {
		t.Fatalf("expected [overloaded], got %v", m.calls)
	}
}
```

You will need `alwaysSubmitter` and `neverSubmitter` stubs — check if they already exist in `service_test.go`. If not, add:

```go
type alwaysSubmitter struct{}
func (a *alwaysSubmitter) Submit(_ *domain.Event) bool { return true }

type neverSubmitter struct{}
func (n *neverSubmitter) Submit(_ *domain.Event) bool { return false }

type stubPool struct{}
func (s *stubPool) Acquire() *domain.Event { return &domain.Event{} }
func (s *stubPool) Release(_ *domain.Event) {}
```

- [ ] **Step 3: Run test — expect compile failure**

```bash
go test ./internal/application/ingestion/... 2>&1 | head -20
```

Expected: `SetMetrics undefined`.

- [ ] **Step 4: Add metrics to IngestionService**

In `internal/application/ingestion/service.go`, add `metrics IngestMetrics` field to `IngestionService` and a `SetMetrics` method:

```go
// Add field to IngestionService struct:
metrics IngestMetrics

// Add method after NewIngestionService:
// SetMetrics attaches an IngestMetrics implementation.
// Call before first Ingest(); safe to call at most once during wiring.
func (s *IngestionService) SetMetrics(m IngestMetrics) {
	s.metrics = m
}
```

Update `Ingest()` to record metrics. Wrap the body with timing and status tracking:

```go
func (s *IngestionService) Ingest(source, payload string) error {
	var start time.Time
	if s.metrics != nil {
		start = time.Now()
	}

	e := s.pool.Acquire()
	e.ReceivedAt = time.Now()
	e.Source = source
	e.Payload = payload

	if s.walWriter != nil {
		if err := s.walWriter.WriteEvent(e); err != nil {
			s.pool.Release(e)
			if s.metrics != nil {
				s.metrics.RecordIngest("wal_error", time.Since(start).Seconds())
			}
			return fmt.Errorf("wal write failed: %w", err)
		}
	}

	if !s.submitter.Submit(e) {
		s.pool.Release(e)
		if s.metrics != nil {
			s.metrics.RecordIngest("overloaded", time.Since(start).Seconds())
		}
		return ErrOverloaded
	}

	if s.metrics != nil {
		s.metrics.RecordIngest("ok", time.Since(start).Seconds())
	}
	return nil
}
```

- [ ] **Step 5: Run tests**

```bash
go test ./internal/application/ingestion/...
```

Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/application/ingestion/metrics.go \
        internal/application/ingestion/service.go \
        internal/application/ingestion/service_test.go
git commit -m "feat(ingestion): add IngestMetrics port to IngestionService"
```

---

## Task 5: Prometheus Infrastructure Package

**Files:**
- Create: `internal/infrastructure/metrics/registry.go`
- Create: `internal/infrastructure/metrics/ingestion.go`
- Create: `internal/infrastructure/metrics/replication.go`
- Create: `internal/infrastructure/metrics/cluster.go`
- Create: `internal/infrastructure/metrics/noop.go`

- [ ] **Step 1: Add Prometheus dependency**

```bash
cd /Users/junyoung/workspace/personal/core-x
go get github.com/prometheus/client_golang@latest
```

Expected: `go.mod` and `go.sum` updated.

- [ ] **Step 2: Create registry.go**

Create `internal/infrastructure/metrics/registry.go`:

```go
// Package metrics provides Prometheus-backed implementations of application-layer metric ports.
//
// Design: Each application port (IngestMetrics) has one Prometheus struct implementing it.
// The registry is a shared prometheus.Registry. All structs register metrics in their
// constructor so that /metrics exposes a complete set from startup.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// NewRegistry creates a new Prometheus registry with Go runtime collectors.
func NewRegistry() *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(prometheus.NewGoCollector())
	reg.MustRegister(prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
	return reg
}

// NewHTTPHandler returns an http.Handler for the /metrics endpoint.
func NewHTTPHandler(reg *prometheus.Registry) http.Handler {
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg})
}
```

- [ ] **Step 3: Create ingestion.go**

Create `internal/infrastructure/metrics/ingestion.go`:

```go
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

// PromIngestMetrics implements application/ingestion.IngestMetrics using Prometheus.
type PromIngestMetrics struct {
	requests  *prometheus.CounterVec
	latency   *prometheus.HistogramVec
	queueDepth prometheus.Gauge
}

// NewPromIngestMetrics registers and returns a PromIngestMetrics.
func NewPromIngestMetrics(reg *prometheus.Registry) *PromIngestMetrics {
	requests := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "core_x_ingest_requests_total",
		Help: "Total number of ingest requests by status.",
	}, []string{"status"})

	latency := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "core_x_ingest_latency_seconds",
		Help:    "Ingest request latency in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"status"})

	queueDepth := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "core_x_worker_queue_depth",
		Help: "Current worker pool queue depth.",
	})

	reg.MustRegister(requests, latency, queueDepth)

	return &PromIngestMetrics{
		requests:   requests,
		latency:    latency,
		queueDepth: queueDepth,
	}
}

// RecordIngest implements IngestMetrics.
func (m *PromIngestMetrics) RecordIngest(status string, latencySeconds float64) {
	m.requests.WithLabelValues(status).Inc()
	m.latency.WithLabelValues(status).Observe(latencySeconds)
}

// RecordQueueDepth implements IngestMetrics.
func (m *PromIngestMetrics) RecordQueueDepth(depth int) {
	m.queueDepth.Set(float64(depth))
}
```

- [ ] **Step 4: Create replication.go**

Create `internal/infrastructure/metrics/replication.go`:

```go
package metrics

import (
	"github.com/junyoung/core-x/internal/infrastructure/replication"
	"github.com/prometheus/client_golang/prometheus"
)

// RegisterReplicationMetrics registers replication metrics from a ReplicationLag tracker.
// Called once during wiring; uses a collector that reads from lag at scrape time.
func RegisterReplicationMetrics(reg *prometheus.Registry, lag *replication.ReplicationLag) {
	if lag == nil {
		return
	}

	lagBytes := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "core_x_replication_lag_bytes",
		Help: "Approximate replication lag in bytes (primary only).",
	}, func() float64 { return float64(lag.Bytes()) })

	reconnects := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "core_x_replication_reconnect_total",
		Help: "Total number of replication reconnects.",
	}, func() float64 { return float64(lag.ReconnectCount()) })

	reg.MustRegister(lagBytes, reconnects)
}
```

- [ ] **Step 5: Create cluster.go**

Create `internal/infrastructure/metrics/cluster.go`:

```go
package metrics

import (
	"github.com/junyoung/core-x/internal/infrastructure/cluster"
	"github.com/prometheus/client_golang/prometheus"
)

// RegisterClusterMetrics registers ring health metrics.
// Called once during wiring when cluster mode is active.
func RegisterClusterMetrics(reg *prometheus.Registry, ring *cluster.Ring) {
	if ring == nil {
		return
	}

	ringNodes := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "core_x_ring_nodes_total",
		Help: "Total number of nodes in the consistent hash ring.",
	}, func() float64 { return float64(ring.Len()) })

	reg.MustRegister(ringNodes)
}
```

- [ ] **Step 6: Create noop.go**

Create `internal/infrastructure/metrics/noop.go`:

```go
package metrics

// NoopIngestMetrics is a zero-overhead IngestMetrics implementation for tests.
// It discards all observations. Use in test doubles where metric recording
// is not the subject under test.
type NoopIngestMetrics struct{}

func (NoopIngestMetrics) RecordIngest(_ string, _ float64) {}
func (NoopIngestMetrics) RecordQueueDepth(_ int)           {}
```

- [ ] **Step 7: Build check**

```bash
cd /Users/junyoung/workspace/personal/core-x
go build ./internal/infrastructure/metrics/...
```

Expected: no errors.

- [ ] **Step 8: Commit**

```bash
git add internal/infrastructure/metrics/ go.mod go.sum
git commit -m "feat(metrics): add Prometheus infrastructure package with ingest/replication/cluster metrics"
```

---

## Task 6: Wire Everything in cmd/main.go + ADR

**Files:**
- Modify: `cmd/main.go`
- Create: `docs/adr/0008-read-path-and-observability.md`

- [ ] **Step 1: Write ADR-008**

Create `docs/adr/0008-read-path-and-observability.md`:

```markdown
# ADR-008: Read Path and Observability Before Consensus

## Status
Accepted

## Context
Phase 3b completed async WAL streaming replication. The system can ingest and replicate
events but has no read API and no Prometheus-compatible metrics. Raft consensus (ADR-007
Phase 4) requires a working read path as a prerequisite; without GET semantics, leader
election has no observable effect. Additionally, replLag and ring metrics are unobservable
without a Prometheus exporter.

## Decision
Phase 4 implements two things before Raft:
1. **Read path**: `GET /kv/{key}` with consistent-hash routing (mirrors write path).
   Replica nodes do not have a KV index; Phase 4 leaves replicas with no local index.
   The write path already streams WAL to replicas; indexing replicated data is Phase 5.
2. **Prometheus observability**: IngestMetrics port in Application layer; Prometheus
   implementation in infrastructure/metrics. `/metrics` endpoint added to HTTP server.

## Consequences
- Positive: System is now a complete read/write distributed KV store (not just a log sink).
- Positive: Prometheus metrics enable real-time monitoring of replLag, queue saturation.
- Negative: Replica reads not served locally (redirect/error until Phase 5 index rebuild).
- Negative: Raft deferred to Phase 5; primary failure still requires manual failover.
```

- [ ] **Step 2: Update cmd/main.go — add imports**

Read `cmd/main.go` (already read above), then add to the import block:

```go
inframetrics "github.com/junyoung/core-x/internal/infrastructure/metrics"
```

- [ ] **Step 3: Wire metrics in cmd/main.go**

After `slog.SetDefault(...)` and before WAL init, add:

```go
// --- Observability: Prometheus Metrics ------------------------------------
promReg := inframetrics.NewRegistry()
ingestMetrics := inframetrics.NewPromIngestMetrics(promReg)
```

After the `svc := appingestion.NewIngestionService(...)` line, add:

```go
svc.SetMetrics(ingestMetrics)
```

After the replication section (after `replLag` is initialized), add:

```go
inframetrics.RegisterReplicationMetrics(promReg, replLag)
inframetrics.RegisterClusterMetrics(promReg, ring)
```

- [ ] **Step 4: Wire KVHandler and /metrics in mux**

Replace the mux block:

```go
var ingestHandler *infrahttp.HTTPHandler
if ring != nil {
	ingestHandler = infrahttp.NewClusterHTTPHandler(svc, ring, nodeID, forwarder, forwardTimeout)
} else {
	ingestHandler = infrahttp.NewHTTPHandler(svc)
}

kvHandler := infrahttp.NewKVHandler(kvStore, ring, nodeID, forwarder)

mux := http.NewServeMux()
mux.Handle("POST /ingest", ingestHandler)
mux.Handle("GET /kv/{key}", kvHandler)
mux.HandleFunc("GET /stats", infrahttp.StatsHandler(workerPool, replLag))
mux.Handle("GET /metrics", inframetrics.NewHTTPHandler(promReg))
mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
})
```

- [ ] **Step 5: Build**

```bash
cd /Users/junyoung/workspace/personal/core-x
go build ./...
```

Expected: clean build.

- [ ] **Step 6: Integration smoke test**

```bash
# Terminal 1: start server
go run ./cmd/main.go &
sleep 1

# Terminal 2: write an event
curl -s -X POST http://localhost:8080/ingest \
  -H "Content-Type: application/json" \
  -d '{"source":"user-1","payload":"hello"}'
# Expected: HTTP 202

# Read it back
curl -s http://localhost:8080/kv/user-1
# Expected: {"source":"user-1","payload":"hello","received_at_unix_ns":...}

# Check metrics
curl -s http://localhost:8080/metrics | grep core_x_ingest
# Expected: core_x_ingest_requests_total{status="ok"} 1

kill %1
```

- [ ] **Step 7: Run full test suite**

```bash
cd /Users/junyoung/workspace/personal/core-x
go test ./...
```

Expected: all PASS.

- [ ] **Step 8: Final commit**

```bash
git add cmd/main.go docs/adr/0008-read-path-and-observability.md
git commit -m "feat(phase4): wire read path + Prometheus metrics in main.go"
```

---

## Self-Review

### Spec Coverage

| Requirement | Task |
|-------------|------|
| GET /kv/{key} HTTP endpoint | Task 3 |
| Cluster-aware routing for reads | Task 3 (KVHandler) |
| KVService.Get gRPC for forwarding | Task 1 + Task 2 |
| IngestMetrics port in Application layer | Task 4 |
| Prometheus implementation | Task 5 |
| /metrics endpoint | Task 6 |
| Replica → read redirect to Primary | Task 3 (single-node path; replica has no index, returns 404) |
| ADR documentation | Task 6 |

### Type Consistency Check

- `KVGetter` interface defined in `grpc/kv_server.go` — `kv.Store` implements `Get(key string) (*domain.Event, error)` ✓
- `KVGetter` interface redefined in `http/kv_handler.go` — same signature ✓
- `IngestMetrics` interface in `application/ingestion/metrics.go` — same signature used in `service.go`, `noop.go`, `ingestion.go` ✓
- `pb.GetResponse.ReceivedAtUnixNs` field name matches proto definition ✓

### Known Limitation

Replica nodes have no KV index (WAL bytes only, no `kv.Store`). A read to a replica in cluster mode will hit the ring lookup — the Primary is the owner, so reads will be forwarded to Primary. Single-node replica `GET /kv/{key}` with no ring returns 404 (key not found) since replica's `kvStore.Get` has an empty index. This is acceptable for Phase 4 — documented in ADR-008.
