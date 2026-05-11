# ADR-021: Storage Unification — Raft-Backed Ingestion Pipeline

**Status:** Accepted
**Date:** 2026-04-26
**Deciders:** junyoung
**Related:** ADR-005 (Bitcask KV), ADR-007 (Consistent Hashing), ADR-013 (Raft Write Path), ADR-014 (KV State Machine), ADR-017 (Snapshot), ADR-018 (Dual Write-Path), ADR-019 (Linearizable Read), ADR-020 (Joint Consensus)

---

## Context

### v1 Dual Write-Path: Intentional Isolation

ADR-018 documented that Core-X deliberately maintains two independent write paths:

```
POST /ingest → ring.Lookup(source) → gRPC forward → Bitcask WAL (per-shard)
POST /raft/kv → Raft consensus → KVStateMachine (in-memory + snapshot)
```

This isolation was the correct call for v1. Each path demonstrated a distinct DDIA chapter:
`/ingest` = Chapter 3 (Storage Engines) + Chapter 6 (Partitioning); `/raft/kv` = Chapter 7–9
(Transactions, Consensus). Merging them prematurely would have obscured the architectural
boundaries that each chapter's concepts depend on.

v1 is complete. All Phase 1–11 objectives have been met. ADR-018 explicitly deferred unification
to v2 and stated: "consistent hashing becomes a routing layer *above* Raft rather than a storage
layer alongside it."

### Current Structural Gap

The two paths are not only operationally separate — they are **semantically divergent**:

| Dimension | Ingestion Path | Raft Path |
|---|---|---|
| Consistency | None (WAL-append only) | Linearizable (ADR-019) |
| Fault tolerance | Single-node per shard | Multi-node quorum |
| Membership change | Static `CORE_X_PEERS` at boot | Dynamic Joint Consensus (ADR-020) |
| Snapshot / recovery | WAL compaction (ADR-006) | Raft snapshot (ADR-017) |
| Read path | Consistent hashing forward | ReadIndex / Lease Read |
| Write durability | Bitcask WAL fsync | WAL-backed Raft log (`WALLogStore`) |

A `POST /ingest` write has **no replication guarantee**. If the shard-owning node crashes before
WAL fsync, the data is lost. If that node restarts after a snapshot, the old Bitcask WAL may not
be replayed against the current ring topology. The ingestion path cannot participate in Joint
Consensus membership changes.

This is the gap ADR-020 §Trade-offs named: "Snapshot membership mismatch — if `ClusterConfig` is
not persisted in snapshots, a node recovering from snapshot will revert to initial static config."
The same class of problem affects the entire ingestion storage layer.

### DDIA Framing

DDIA Chapter 9 §Linearizability: "If one operation completes before another begins, the later
operation must observe the result of the earlier." The current ingestion path provides no such
guarantee. DDIA Chapter 5 §Replication: "Single-leader replication — if the leader fails before
replicating a write, the write is lost." The current Bitcask shards are effectively single-leader
with no replication.

| DDIA Pillar | Current Gap | Target State |
|---|---|---|
| **Reliability** | Ingestion writes not replicated; node crash = data loss | All writes go through Raft; minimum quorum durability |
| **Scalability** | Two codepaths to scale independently; Bitcask shards cannot rebalance under Joint Consensus | Single write path; Consistent Hashing ring drives Raft group assignment; ring changes are Raft log entries |
| **Maintainability** | Two storage backends, two WAL formats, two recovery paths, two test suites | One backend (`KVStateMachine` + `WALLogStore`); one recovery path; one snapshot format |

---

## Decision

**Promote Consistent Hashing from a storage-routing layer to a pure partitioning layer. Make Raft
the sole consensus and storage backend for all writes.**

The new unified write path:

```
POST /ingest → ring.Lookup(source) → RaftGroup.Leader(partition) → Propose → Apply → KVStateMachine
GET  /kv/{key} → ring.Lookup(key) → RaftGroup.Leader(partition) → ReadIndex → sm.Get(key)
```

Consistent Hashing answers: "which partition owns this key?"
Raft answers: "how does that partition durably commit a write?"

This is the architecture of TiKV (Raft Groups per Region), CockroachDB (Raft Groups per Range),
and etcd (single Raft group). Core-X implements the conceptually identical structure at smaller
scale.

---

## Design

### 1. Partition → Raft Group Mapping

Each ring partition is backed by a dedicated `RaftNode` instance. In the current codebase a
`RaftNode` manages a single Raft group. Phase 12 introduces a `RaftGroupRegistry` that maps
a partition ID to the `RaftNode` responsible for it:

```go
// internal/infrastructure/raft/group_registry.go

// PartitionID identifies a ring partition. Derived from ring.Lookup(key).ID.
type PartitionID string

// RaftGroupRegistry maps partition IDs to their managing RaftNode.
// In Phase 12 this is a static 1:1 mapping (one RaftNode per physical node,
// node ID == partition ID). Multi-group expansion is deferred to Phase 13.
type RaftGroupRegistry struct {
    mu     sync.RWMutex
    groups map[PartitionID]*RaftNode
}

// Get returns the RaftNode responsible for partitionID, and whether it exists.
func (r *RaftGroupRegistry) Get(id PartitionID) (*RaftNode, bool) {
    r.mu.RLock()
    defer r.mu.RUnlock()
    n, ok := r.groups[id]
    return n, ok
}

// Register adds a RaftNode for partitionID.
// Must be called before Serve; not safe for concurrent use during registry setup.
func (r *RaftGroupRegistry) Register(id PartitionID, node *RaftNode) {
    r.mu.Lock()
    defer r.mu.Unlock()
    r.groups[id] = node
}
```

**Phase 12 scope:** 1 physical node = 1 Raft group = 1 partition. The ring topology remains
unchanged (3 nodes, 150 vnodes each). `ring.Lookup(key)` returns a `*cluster.Node` whose ID is
used as the `PartitionID`.

### 2. Unified Ingestion Handler

`HTTPHandler.ServeHTTP` (`internal/infrastructure/http/handler.go`) is extended to replace the
`IngestionService.Ingest` call with a `RaftGroupRegistry.Propose` call when the node is the
ring-responsible leader. Non-leader nodes forward via gRPC as before (routing is unchanged).

```go
// internal/infrastructure/http/handler.go (modified)

type HTTPHandler struct {
    // Existing fields:
    ring           *cluster.Ring
    selfID         string
    forwarder      *infragrpc.Forwarder
    forwardTimeout time.Duration

    // Phase 12: replaces svc *appingestion.IngestionService on the write path.
    // When non-nil, writes go through Raft. When nil, falls back to legacy path
    // (used only during integration-test bringup).
    registry *infraraft.RaftGroupRegistry
}

// ServeHTTP — write path (POST /ingest):
//
//  ring.Lookup(source) → target node
//  if target != self → gRPC forward (unchanged from Phase 3)
//  if target == self:
//    partition := PartitionID(target.ID)
//    node, ok := registry.Get(partition)
//    if !ok → 503
//    cmd := RaftKVCommand{Op: "set", Key: source, Value: payload}
//    index, _, isLeader := node.Propose(marshalCmd(cmd))
//    if !isLeader → redirect or 503
//    sm.WaitForIndex(ctx, index) → 204 or 504
```

The `IngestionService` and Bitcask `Store` are no longer on the write hot path. They are retained
for the WAL compaction background job and for reads during the transition period (see §Migration
Strategy).

### 3. Unified Read Handler

`GET /kv/{key}` is served by a new `UnifiedKVHandler` that replaces both
`KVHandler` (Bitcask) and `RaftKVGetHandler` (Raft) with a single routing layer:

```go
// internal/infrastructure/http/unified_kv_handler.go

// UnifiedKVHandler routes GET /kv/{key} through Raft ReadIndex.
// ring.Lookup(key) → partition owner → ReadIndex → sm.Get(key)
//
// If this node is not the owner:
//   - gRPC forward to owner (owner runs ReadIndex)
//
// If this node is the owner but not the leader:
//   - HTTP redirect to the Raft leader for that partition (307)
type UnifiedKVHandler struct {
    ring           *cluster.Ring
    selfID         string
    registry       *infraraft.RaftGroupRegistry
    forwarder      *infragrpc.Forwarder
    addrMap        map[string]string // nodeID → HTTP base URL
    forwardTimeout time.Duration
}
```

Read flow:

```
GET /kv/{key}
  │
  ├─ ring.Lookup(key) → ownerNode
  ├─ ownerNode.ID != selfID → ForwardGet(ownerNode, key) [gRPC]
  │
  └─ ownerNode.ID == selfID
       │
       ├─ registry.Get(partitionID) → raftNode
       ├─ raftNode.ReadIndex(ctx) → readIndex   [quorum heartbeat or lease]
       ├─ sm.WaitForIndex(ctx, readIndex)
       └─ sm.Get(key) → 200 JSON | 404
```

This gives `GET /kv/{key}` the same linearizability guarantee as `GET /raft/kv/{key}` (ADR-019),
but routed through the consistent hash ring rather than requiring the caller to know which node
is the Raft leader.

### 4. ClusterConfig Persistence in Snapshots (Known Gap Closure)

ADR-020 §Trade-offs explicitly deferred this: "Include `ClusterConfig` in `SnapshotMeta` in
Phase 12." The gap is: after `InstallSnapshot`, a node reverts to its static boot config and
may compute the wrong quorum.

`snapshot.go` already defines the required fields (as of the ADR-020 implementation):

```go
// Already present in snapshot.go (Phase 11):
type SnapshotMeta struct {
    Index         int64
    Term          int64
    CreatedAt     time.Time
    Size          int64
    CRC32         uint32
    ClusterConfig ClusterConfig // active membership at snapshot time
}

type SnapshotData struct {
    KV     map[string]string
    Config ClusterConfig // embedded in KV map under snapshotConfigKey
}
```

The gap is in the restore path. `RaftNode.handleInstallSnapshot` must read `meta.ClusterConfig`
and call `applyConfigEntry`-equivalent logic after restoring the state machine:

```go
// internal/infrastructure/raft/node.go (modified restore path)

func (n *RaftNode) restoreSnapshot(meta SnapshotMeta, data SnapshotData) error {
    if err := n.sm.RestoreSnapshot(data, meta.Index); err != nil {
        return err
    }

    // Restore ClusterConfig from snapshot meta so that quorum calculations
    // after restore reflect the membership at the time the snapshot was taken.
    // Without this, a restarted node reverts to its initial static peer list
    // and may miscalculate quorum size (ADR-020 known gap, Phase 12 closure).
    n.mu.Lock()
    if !meta.ClusterConfig.IsZero() {
        n.clusterConfig = meta.ClusterConfig
        n.peers.EnsureConnected(allVoters(n.clusterConfig))
        slog.Info("raft: cluster config restored from snapshot",
            "voters", n.clusterConfig.Voters,
            "phase", n.clusterConfig.Phase)
    }
    n.mu.Unlock()

    return nil
}
```

`ClusterConfig.IsZero()` returns true when `Voters` is nil/empty — this handles snapshots
written before Phase 12 (backward compatibility: old snapshots have zero config, node falls
back to static boot peers as before).

### 5. RaftKVCommand Schema Extension

The existing `RaftKVCommand` (used by `/raft/kv`) is reused for ingestion writes without change.
The `source` field from `POST /ingest` maps to `Key`; `payload` maps to `Value`.

```go
// Existing type — no change required:
type RaftKVCommand struct {
    Op    string `json:"op"`    // "set" | "del"
    Key   string `json:"key"`   // maps from ingestRequest.Source
    Value string `json:"value"` // maps from ingestRequest.Payload
}
```

This is intentional: the Raft log becomes the single source of truth for all KV operations
regardless of entry point. A replayed WAL produces the same `KVStateMachine` state whether the
original write came from `/ingest` or `/raft/kv`.

### 6. Legacy Path Deprecation and Removal

Phase 12 proceeds in two sub-phases to allow incremental validation:

**Phase 12a (parallel paths, validation):**
- New unified path is live: `POST /ingest` → Raft → `KVStateMachine`
- Legacy path (`IngestionService` + Bitcask) remains wired up but gated behind a feature flag:
  `CORE_X_STORAGE_UNIFIED=false` (default `true` once tests pass)
- Dual-write integration test: same key written via both paths; Raft path must win on read

**Phase 12b (legacy removal):**
- `IngestionService`, `appingestion` package, and Bitcask `Store` references removed from
  the write path
- `HTTPHandler.svc` field removed; `HTTPHandler.registry` is the only write backend
- `GET /kv/{key}` routes exclusively through `UnifiedKVHandler`; `KVHandler` (Bitcask) removed
- `POST /raft/kv` retained for direct Raft access (useful in testing and ops tooling)

**What is NOT removed in Phase 12:**
- `internal/infrastructure/storage/kv` package — retained for `KVDurableStore` interface
  implementation by `KVStateMachine`'s durable backend
- `internal/infrastructure/storage/wal` package — retained; `WALLogStore` depends on it
- Bitcask compaction and WAL reader code — retained as infrastructure primitives

---

## Concurrency and Goroutine Topology

The write path adds no new goroutines. `Propose` is a mutex-guarded call that returns
`(index, term, isLeader)` synchronously. `WaitForIndex` parks the HTTP handler goroutine on a
channel until the apply loop notifies it — the same pattern already proven in the Raft KV path
(ADR-014).

The apply loop goroutine topology is unchanged:

```
HTTP goroutine (per-request)
  │  Propose(data) → returns (index, _, isLeader)
  │  WaitForIndex(ctx, index) → parks on ch
  │
RaftNode.runApplyLoop goroutine
  │  commitIndex advances → sends LogEntry to applyCh
  │
KVStateMachine.Run goroutine
  │  apply(entry) → write Bitcask → update sm.data → lastApplied.Store(index) → close(ch)
  │
HTTP goroutine (unparked) → 204 response
```

Backpressure: `applyCh` has buffer 256. If the state machine apply loop falls behind (e.g., slow
Bitcask fsync), `runApplyLoop` blocks on `applyCh <- entry`. This bounds in-flight entries at 256
and applies backpressure to `Propose` callers (they wait in `WaitForIndex`). Downstream saturation
does not cascade to Raft log replication — the leader continues committing and `lastApplied` will
catch up when I/O resumes.

**Goroutine leak audit:** `WaitForIndex` registers a waiter channel and removes it in the
`ctx.Done()` branch — no leak on timeout. `ReadIndex` spawns no goroutines; it parks on an
internal response channel with the `ctx` deadline enforced by `select`. `ProposeConfigChange`
spawns one background goroutine per membership change, bounded by `configChangeTimeout = 30s`.

---

## Trade-off Analysis

### What We Are Optimising For

**Reliability** (primary): all writes are replicated to a Raft quorum before the HTTP 204
response is sent. A single node crash cannot cause data loss.

**Maintainability** (secondary): one storage backend, one recovery path, one snapshot format.
New contributors need to understand one write path, not two.

### What We Are Sacrificing

| Dimension | Cost | Acceptability |
|---|---|---|
| **Write latency** | `POST /ingest` p99 increases from ~0.5 ms (single WAL fsync) to ~2–5 ms (Raft round-trip + quorum fsync). Single-digit ms is acceptable for the target use case. | Acceptable |
| **Throughput ceiling** | Raft serialises writes through the leader; throughput per partition is bounded by leader's WAL sequential write speed. Current WALWriter throughput: ~80k entries/s (Phase 1 benchmark). Raft overhead: ~15% at 3-node, 50ms heartbeat. Effective ceiling: ~68k entries/s per partition. | Acceptable |
| **Operational complexity** | Deploying Phase 12 requires all nodes to upgrade together (Raft log entry schema is unchanged; `KVStateMachine` backend is unchanged; only the HTTP routing layer changes). Rolling upgrade is safe. | Low risk |
| **Feature flag period** | Phase 12a introduces a `CORE_X_STORAGE_UNIFIED` flag. Two code paths exist simultaneously for a short window. Flag must be removed in Phase 12b; stale flags are a maintainability liability. | Time-bounded |

### When This Trade-off Breaks

1. **Single-partition throughput exceeds ~68k writes/s**: the leader's WAL becomes the bottleneck.
   Mitigation: multi-group Raft (Phase 13) — split the keyspace across multiple Raft groups per
   physical node, each with its own WAL and apply loop.

2. **Network RTT between nodes exceeds ~20ms**: Raft heartbeat interval (currently 50ms) must be
   tuned to 5–10× RTT. If `heartbeatTimeout < 5 × RTT`, spurious leader elections cause latency
   spikes. Mitigation: expose `CORE_X_RAFT_HEARTBEAT_MS` env var and adjust per deployment.

3. **State machine apply falls behind by >256 entries**: `applyCh` fills, `runApplyLoop` blocks,
   `Propose` callers queue up. Under sustained overload, `WaitForIndex` callers time out (504).
   Monitoring signal: `applyCh` depth metric. Mitigation: increase `applyCh` buffer (Phase 13
   tuning) or add write-path admission control (reject at 429 before `Propose`).

4. **ClusterConfig snapshot gap not fully closed**: if a node running Phase 11 code (without the
   Phase 12 restore fix) receives a snapshot taken by a Phase 12 node, it will ignore the
   embedded `ClusterConfig` (the `IsZero()` branch falls through). This is safe — the node reverts
   to static boot config — but it may compute a smaller quorum than expected.
   Mitigation: Phase 12 is a coordinated upgrade; all nodes should run Phase 12 code before
   membership changes are attempted.

---

## Implementation Plan

The implementation is structured as four sequential steps. Each step passes the full test suite
before the next begins.

### Step 1 — Close ClusterConfig Snapshot Gap (ADR-020 Known Gap)

**Goal:** `restoreSnapshot` applies `meta.ClusterConfig` to `n.clusterConfig` after restore.
This is the prerequisite for all subsequent steps: without it, a node recovering from snapshot
in a unified cluster has an incorrect membership view.

**Files modified:**
- `internal/infrastructure/raft/node.go` — `restoreSnapshot` reads `meta.ClusterConfig`;
  calls `n.peers.EnsureConnected` for any new peers
- `internal/infrastructure/raft/cluster_config.go` — add `IsZero() bool` method to
  `ClusterConfig` (returns `len(Voters) == 0`)

**Test:**
- `snapshot_e2e_test.go`: after snapshot restore, assert `n.clusterConfig` matches the config
  embedded in `SnapshotMeta`

**Validation gate:** all existing snapshot E2E tests pass; `clusterConfig` is non-zero after restore.

### Step 2 — `RaftGroupRegistry`

**Goal:** introduce the `PartitionID → *RaftNode` mapping without changing any HTTP routing.

**Files created:**
- `internal/infrastructure/raft/group_registry.go` — `RaftGroupRegistry` type with
  `Register`, `Get`, `All` methods

**Files modified:**
- `cmd/main.go` — instantiate `RaftGroupRegistry`; call `registry.Register(selfID, raftNode)`

**Validation gate:** unit tests for `RaftGroupRegistry`; no HTTP behaviour change.

### Step 3 — Phase 12a: Unified Write Path (Feature-Flagged)

**Goal:** `POST /ingest` routes through Raft when `CORE_X_STORAGE_UNIFIED=true`.

**Files modified:**
- `internal/infrastructure/http/handler.go` — add `registry *infraraft.RaftGroupRegistry` field;
  in `ServeHTTP`, when `registry != nil` and `CORE_X_STORAGE_UNIFIED=true`, call
  `registry.Get(partitionID)` → `node.Propose` → `sm.WaitForIndex`
- `cmd/main.go` — wire `registry` into `HTTPHandler`; read `CORE_X_STORAGE_UNIFIED` env var

**Files created:**
- `internal/infrastructure/http/unified_kv_handler.go` — `UnifiedKVHandler` type (see §Design §3)
- `internal/infrastructure/http/unified_kv_handler_test.go`

**Integration test (new):**
- `storage_unification_test.go` — 3-node cluster, write via `POST /ingest`, read via
  `GET /kv/{key}` (unified path), assert value matches; kill leader mid-write, assert no data
  loss after re-election

**Validation gate:** `CORE_X_STORAGE_UNIFIED=true` passes integration tests; `=false` falls
back to legacy path (no regression).

### Step 4 — Phase 12b: Legacy Path Removal

**Goal:** remove dead code; `IngestionService` no longer on write path.

**Files modified:**
- `internal/infrastructure/http/handler.go` — remove `svc *appingestion.IngestionService` field
  and all call sites; `registry` is the only write backend
- `internal/infrastructure/http/kv_handler.go` — replace `KVHandler` registration in
  `cmd/main.go` with `UnifiedKVHandler`; `KVHandler` file retained but not registered
- `cmd/main.go` — remove `CORE_X_STORAGE_UNIFIED` flag; unified path is unconditional;
  remove `IngestionService` wiring from the write path

**Files NOT removed:**
- `internal/application/ingestion/` — retained (may be used by future batch-ingest use cases)
- `internal/infrastructure/storage/kv/` — retained (`KVDurableStore` interface and `Store` type)
- `internal/infrastructure/storage/wal/` — retained (`WALLogStore` dependency)

**Validation gate:** full test suite passes with zero references to `svc.Ingest` in the HTTP
handler write path; `go build ./...` clean; `go vet ./...` clean.

---

## Safety Analysis

### Write Path Linearizability

After Phase 12, `POST /ingest` provides the same linearizability guarantee as `POST /raft/kv`
(ADR-013, ADR-016). The proof is identical: `Propose` returns only after the entry is appended
to the leader's log; `WaitForIndex` returns only after the entry is committed and applied to
`KVStateMachine`. The HTTP 204 response is therefore a linearizability point.

### Backward Compatibility

The `RaftKVCommand` wire format is unchanged. Existing Raft log entries replayed from WAL are
unaffected. Snapshot format gains no new fields (the `ClusterConfig` field in `SnapshotMeta`
already exists as of Phase 11; Phase 12 only changes how it is consumed on restore).

### Leader Redirect on Ingest

If `ring.Lookup(source)` points to this node but this node is not the Raft leader (e.g.,
immediately after a leader election), `Propose` returns `(0, 0, false)`. The handler checks
`isLeader` and redirects (307) to the known leader's HTTP address via `addrMap`. The client
retries transparently. This is the same pattern as `ProposeHandler` (ADR-013).

### Partition during Write

If the leader is partitioned after `Propose` appends the entry but before it commits, the entry
may or may not be committed by the new leader (Raft §5.4.2: only entries from the current term
are committed by counting). The HTTP handler's `WaitForIndex` times out (504). The client retries.
If the new leader elected after partition has the entry in its log, it will eventually commit and
apply it (leader completeness §5.4.1). If not, the client's retry writes a new entry. Either way,
no duplicate apply occurs because `KVStateMachine.apply` is idempotent for `set` operations
(last-write-wins semantics).

---

## Consequences

### What Changes

1. `POST /ingest` is backed by Raft consensus. Writes are replicated to a quorum before ACK.
2. `GET /kv/{key}` uses `ReadIndex` (ADR-019) for linearizable reads, regardless of path.
3. `ClusterConfig` is restored from snapshots — ADR-020 known gap is closed.
4. `HTTPHandler` no longer depends on `appingestion.IngestionService` on the hot write path.
5. One new type: `RaftGroupRegistry` maps partitions to `RaftNode` instances.

### What Does Not Change

1. Ring topology and `ring.Lookup` semantics — unchanged.
2. gRPC forwarding path for non-owning nodes — unchanged.
3. `KVStateMachine`, `WALLogStore`, `SnapshotStore` internals — unchanged.
4. `POST /raft/kv` and `GET /raft/kv/{key}` endpoints — retained.
5. Snapshot format on disk — unchanged (field was already present in `SnapshotMeta`).

### ADRs Superseded

ADR-018 (Intentional Dual Write-Path) is **superseded** by this ADR once Phase 12b is complete.
The dual path was intentional and correct for v1; Phase 12 is the planned unification.

### Risks and Mitigations

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| Write latency regression visible to load tests | Medium | Medium | Phase 12a dual-path allows benchmark comparison before legacy removal |
| `applyCh` saturation under burst ingest | Low | Medium | Monitor `applyCh` depth; add 429 admission control if needed |
| Rolling upgrade: Phase 11 node receives Phase 12 snapshot | Low | Low | `IsZero()` check; node falls back to static boot config safely |
| Feature flag left in codebase after Phase 12b | Low | Low | Phase 12b PR explicitly removes `CORE_X_STORAGE_UNIFIED` flag |
| Leader redirect loop (redirect → follower → redirect) | Very Low | Low | `addrMap` is populated at boot; leader ID comes from `n.LeaderID()`; same guard as ADR-013 |

---

## Monitoring and Validation

### Monitoring Signals

| Signal | Type | Alert Condition |
|---|---|---|
| `raft_propose_total{path="ingest"}` | counter | Rate == 0 when `POST /ingest` traffic is non-zero → routing broken |
| `raft_apply_lag` (commitIndex - lastApplied) | gauge | > 100 sustained → apply loop falling behind; risk of `WaitForIndex` timeouts |
| `ingest_apply_ch_depth` | gauge | > 200 → approaching `applyCh` saturation (buffer = 256) |
| `ingest_write_duration_seconds` p99 | histogram | > 20ms → Raft round-trip unusually slow; check heartbeat interval vs RTT |
| `snapshot_cluster_config_restored_total` | counter | 0 after rolling restart with membership change → gap not closed |
| `raft_leader_redirect_total{path="ingest"}` | counter | High rate → ring topology and Raft leader are frequently misaligned |

### Validation Criteria

The design is considered validated when:

1. All existing unit, integration, and chaos tests pass without modification (regression-free).
2. `POST /ingest` with `CORE_X_STORAGE_UNIFIED=true`: 3-node cluster, kill leader mid-write,
   assert data present after re-election (no data loss).
3. `GET /kv/{key}` after unified write returns consistent value (no stale read) under
   network partition scenario (mirrors ADR-019 validation test).
4. Snapshot restore test: take snapshot with 3-node config, restore on new node, assert
   `clusterConfig.Voters` == 3-node list (ADR-020 gap closed).
5. `go tool pprof`: p99 allocation per `POST /ingest` request ≤ 3 heap allocations
   (JSON marshal × 1, channel receive × 1, response write × 1) — no regression from Phase 1
   allocation budget.
6. **Performance baseline comparison (Phase 12a):** Under identical load (50k RPS, 60s sustained),
   measure Direct Write (legacy) and Unified Write (Raft) side-by-side. Record:
   - p99 latency ratio (expected: Raft path ~3–5× slower due to quorum fsync)
   - `raft_apply_lag` under load (threshold: < 100 sustained)
   - throughput ceiling where `applyCh` depth exceeds 200
   The goal is not to hit a specific number but to **explain the delta**. "Raft overhead added
   Xms p99, caused by Y" is the validation — not "p99 < 10ms".
