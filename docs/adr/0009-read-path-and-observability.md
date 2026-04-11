# ADR-008: Read Path and Observability Before Consensus

## Status
Accepted

## Context
Phase 3b completed async WAL streaming replication. The system can ingest and replicate
events but has two gaps:
1. No read API — `kv.Store.Get()` exists but `GET /kv/{key}` is not exposed.
2. No Prometheus-compatible metrics — `/stats` returns JSON but can't feed a Prometheus scrape pipeline.

ADR-007 Phase 4 originally planned Raft consensus, but Raft requires a working read path
as a prerequisite (leader election has no observable effect without GET semantics).
Additionally, replLag and ring metrics are unobservable without a Prometheus exporter.

## Decision

### Phase 4 implements two things before Raft:

**1. Read path: `GET /kv/{key}`**
- Cluster routing mirrors write path: `ring.Lookup(key)` -> local or gRPC forward
- New `KVService.Get` proto RPC for inter-node forwarding
- Replica nodes have no KV index (WAL bytes only); reads are forwarded to Primary via ring lookup
- Phase 5 will add index rebuild on replica for local reads

**2. Prometheus observability**
- `IngestMetrics` port in Application layer (no Prometheus import in application code)
- `infrastructure/metrics` package implements the port with Prometheus
- `/metrics` endpoint added to HTTP server
- Replication lag and cluster ring size exposed as GaugeFuncs (read at scrape time)

### Why not Raft first?
- Read path without Raft: works immediately, observable via Prometheus
- Raft without read path: consensus exists but results cannot be queried
- Correct sequencing: Read path -> Observability -> Raft (Phase 5)

### Why Prometheus over custom metrics?
- Industry standard; feeds Grafana, alerting pipelines without custom tooling
- `client_golang` adds ~1MB binary; acceptable given gRPC dependency already exists
- ADR-001 exception rationale: educational value is in the metric design, not re-implementing a Prometheus client

## Consequences

**Positive:**
- Core-X is now a complete read/write distributed KV store, not just an ingestion sink
- replLag, queue saturation, ring health are observable in real time
- Prometheus enables load test and chaos test result analysis (Phase 3b tools)

**Negative:**
- Replica read path: replica nodes return 404 for GET /kv/{key} in single-node mode (no index). In cluster mode, ring routes reads to Primary automatically.
- Raft deferred to Phase 5; primary failure still requires manual failover

**Monitoring signals:**
- `core_x_replication_lag_bytes > 1MB` -> Replica falling behind, check Primary I/O
- `core_x_worker_queue_depth / max_depth > 0.8` -> 429 imminent, scale workers
