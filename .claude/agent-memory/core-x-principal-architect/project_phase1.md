---
name: Phase 1 Architecture
description: Zero-allocation HTTP ingestion engine design decisions: pool, worker pool, backpressure, graceful shutdown, Clean Architecture layering
type: project
---

## Status: Phase 2 WAL integration COMPLETE (2026-04-07)

Old `internal/ingestion/` package fully deleted. New layout is live and compiles clean.

## Established patterns (Phase 1)

**Package layout (post-Clean Architecture refactor):**
- `internal/domain/` — Event struct, EventProcessor interface, EventProcessorFunc adapter
- `internal/application/ingestion/` — IngestionService (use case), Submitter/Stats/EventPool ports
- `internal/infrastructure/http/` — HTTP handler, stats handler
- `internal/infrastructure/pool/` — sync.Pool wrapper
- `internal/infrastructure/executor/` — WorkerPool (goroutine scheduler)
- `cmd/main.go` — Composition Root; only place that knows all concrete types

**Dependency rule:**
- Infrastructure → Application → Domain
- Infrastructure → Domain
- Domain imports nothing outside stdlib (only `time`)

**Interface cost decisions (explicit):**
- `EventProcessor` interface: ~2ns itab dispatch per call. Called inside worker goroutine, NOT on HTTP accept path. Acceptable.
- `Submitter` port in IngestionService: ~2ns. Called once per request after JSON decode (~500ns). Negligible.
- `EventPool` port in IngestionService: ~2ns per Acquire/Release. Acceptable in Phase 1; convert to concrete type if profiling shows otherwise.
- HTTP Handler holds `*appingestion.IngestionService` as concrete type (no interface dispatch on HTTP hot path).

**sync.Pool usage:**
- `AcquireEvent()` / `ReleaseEvent()` — typed wrappers around `sync.Pool`, lives in `infrastructure/pool/`
- `Event.Reset()` (exported) zeroes all fields before `Put()` — called by pool, not by callers directly
- Pool chosen over channel-based free list because sync.Pool drains under GC pressure (no permanent memory hold)

**Worker pool:**
- Fixed goroutine count: `runtime.NumCPU() * 2` default (I/O-bound assumption)
- Buffered channel depth: `numWorkers * 10` default
- `processedCount` via `atomic.Int64` — zero lock contention on hot path
- `QueueDepth()` via `len(jobCh)` — for Phase 4 Prometheus metrics
- `processor` field changed from `func(*Event)` to `domain.EventProcessor` interface

**ReleaseEvent location decision:**
- `executor.work()` calls `pool.ReleaseEvent(e)` after `processor.Process(e)`
- executor imports infrastructure/pool — same layer, acceptable coupling
- Domain/Application must NOT import pool (would violate dependency rule)

**Backpressure:**
- `Submit()` is non-blocking (select/default pattern)
- Returns false → IngestionService returns ErrOverloaded → handler returns HTTP 429
- Error sentinel values (not fmt.Errorf wrappers) avoid allocation on rejection path

**Graceful shutdown order:**
1. `server.Shutdown(ctx)` — stop accepting new HTTP connections
2. `pool.Shutdown(ctx)` — drain channel then stop workers
- Order is critical: inverting it creates a race where in-flight submits land on a closed channel (panic)

**HTTP server timeouts (all explicit):**
- ReadHeaderTimeout: 5s (Slowloris defense)
- ReadTimeout: 10s, WriteTimeout: 10s, IdleTimeout: 60s

**Wire format vs internal type separation:**
- `ingestRequest` (wire, in infrastructure/http) vs `Event` (domain)
- Allows independent evolution; Phase 3 protobuf migration touches only infrastructure

**WAL integration (Phase 2, COMPLETE):**
- `infrastructure/storage/wal.Writer` implements `application/ingestion.WALWriter` port (`WriteEvent(*domain.Event) error`)
- `EncodeEvent` in `encode.go` is package-exported (capital E); `writer.go` calls `EncodeEvent` — was a bug (`encodeEvent` undefined) fixed during Phase 2 integration
- WAL initialized before all other infra in `cmd/main.go`; fatal `os.Exit(1)` if file cannot be opened
- Default path: `./data/events.wal`; env override: `CORE_X_WAL_PATH`; dir auto-created via `os.MkdirAll` with 0750 perms
- SyncPolicy default: `SyncInterval` 100ms (balance of durability vs throughput)
- Shutdown order: HTTP stop → `walWriter.Close()` (fsync) → `workerPool.Shutdown()`
  - Rationale: after HTTP stop, no new `WriteEvent` calls possible; WAL close before worker drain is safe because `Ingest()` guarantees WAL write precedes `Submit()`

**Phase extension hooks:**
- Phase 3: add `infrastructure/grpc/handler.go` reusing same `IngestionService`
- Phase 4: replace `infrastructure/http/stats.go` with Prometheus handler; same `Stats` port

**Performance baseline targets (to be validated by benchmark):**
- Allocation goal: ≤1 heap alloc per request (the json.Decoder itself)
- If Decoder allocation is confirmed hot: evaluate bytedance/sonic or pooled Decoder
- Interface dispatch budget: ≤3 itab lookups per request path (Submitter + EventPool x2)
