# ADR-022: MVCC (Multi-Version Concurrency Control)

**Status:** Accepted
**Date:** 2026-05-11
**Deciders:** junyoung
**Related:** ADR-014 (KV State Machine), ADR-019 (Linearizable Read), ADR-021 (Storage Unification)

---

## Context

### Current Write Path and Its Correctness Gap

`KVStateMachine` maintains a single-version map:

```go
type KVStateMachine struct {
    data map[string]string // key → latest value only
    ...
}
```

A write unconditionally overwrites the previous value:

```go
case "set":
    sm.data[cmd.Key] = cmd.Value  // previous version discarded
```

This produces a **lost update** in the following scenario:

```
Timeline:
  T0: key="stock", value="100"
  T1: client-A: GET stock → "100"
  T2: client-B: GET stock → "100"
  T3: client-A: SET stock = "90"  (100 - 10, write committed)
  T4: client-B: SET stock = "80"  (100 - 20, write committed)
  T5: GET stock → "80"

  Expected: "70" (both deductions applied)
  Actual:   "80" (client-A's write lost)
```

Both clients read the same version and independently wrote back an updated value.
Neither knew the other had already modified it.

This violates the **Lost Update** prevention requirement from DDIA Chapter 7
("Transactions").

### Why Raft alone does not solve this

Raft serialises writes through the leader and guarantees that both `SET stock=90`
and `SET stock=80` are committed in some order. But it serialises the
*mutations*, not the *read-modify-write cycles*. Client-B's cycle started from
a stale read, so its write is semantically wrong even though it is technically
committed.

### DDIA Framing

Per DDIA Chapter 7 §"Preventing Lost Updates":

> The lost update problem can occur if an application reads some value from the
> database, modifies it, and writes back the modified value (a read-modify-write
> cycle). If two transactions do this concurrently, one of the modifications can
> be lost.

Solutions described in DDIA §7.2:
1. **Atomic write operations** — `UPDATE counter SET value = value - 10`
2. **Explicit locking** — `SELECT FOR UPDATE`
3. **Automatically detecting lost updates** — database detects and aborts one tx
4. **Compare-and-set (CAS)** — write only if value hasn't changed since read

This ADR implements **option 4 (CAS)** via MVCC versioning, because:
- It requires no lock held across round-trips (better throughput)
- It fits naturally into the existing Raft Propose path
- It is the basis for optimistic concurrency control in PostgreSQL, CockroachDB

---

## Decision

Introduce **MVCC versioning** into `KVStateMachine`:

1. Each key carries a monotonically increasing version counter.
2. Writes accept an optional `expected_version` field (CAS).
3. If `expected_version` is set and does not match the current version → the
   command is a no-op and the apply loop signals a conflict (409).
4. Reads can request a specific version for **snapshot reads**.
5. Old versions are retained up to a configurable limit and garbage-collected.

---

## Design

### Data model

```go
// MVCCVersion is a single immutable snapshot of a key's value.
type MVCCVersion struct {
    Version uint64 // monotonically increasing per key
    Value   string
    Deleted bool
}

// MVCCStateMachine replaces KVStateMachine.
type MVCCStateMachine struct {
    mu       sync.RWMutex
    versions map[string][]MVCCVersion // key → versions, oldest first
    latest   map[string]uint64        // key → current version number

    // retention controls how many versions to keep per key.
    // 0 means keep all (unbounded; for tests).
    retention int

    waitMu      sync.Mutex
    waiters     map[int64][]chan struct{}
    lastApplied atomic.Int64
}
```

### Command wire format

`RaftKVCommand` gains one new field:

```go
type RaftKVCommand struct {
    Op              string `json:"op"`               // "set" | "del" | "cas"
    Key             string `json:"key"`
    Value           string `json:"value"`
    ExpectedVersion uint64 `json:"expected_version"` // 0 = unconditional write
}
```

`expected_version == 0` → unconditional write (backward-compatible with
existing "set" / "del" ops).

`expected_version > 0` and `op == "cas"` → write only if current version
matches; conflict otherwise.

### Apply logic

```
apply(entry):
  cmd = parse(entry.Data)

  if cmd.ExpectedVersion > 0:
    current = latest[cmd.Key]  // 0 if key does not exist
    if current != cmd.ExpectedVersion:
      conflictStore[entry.Index] = ConflictResult{Key: cmd.Key, ...}
      notifyWaiters(entry.Index)  // unblock HTTP handler; handler reads conflict
      return                      // version mismatch — do not write

  nextVersion = latest[cmd.Key] + 1
  versions[cmd.Key] = append(versions[cmd.Key], MVCCVersion{
      Version: nextVersion, Value: cmd.Value, Deleted: (cmd.Op == "del"),
  })
  latest[cmd.Key] = nextVersion
  gc(cmd.Key)  // trim old versions if len > retention
  lastApplied.Store(entry.Index)
  notifyWaiters(entry.Index)
```

### HTTP API changes

| Method | Path | Behaviour |
|---|---|---|
| `GET` | `/kv/{key}` | Returns latest version (linearizable via ReadIndex) |
| `GET` | `/kv/{key}?version=N` | Returns version N (snapshot read; no ReadIndex needed) |
| `POST` | `/ingest` | Unconditional write (backward-compatible) |
| `PUT` | `/kv/{key}` | CAS write: body `{"value":"…","expected_version":N}` |

`PUT /kv/{key}` response codes:
- `204 No Content` — CAS succeeded
- `409 Conflict` — version mismatch; body contains `{"current_version": N}`

### Snapshot and recovery

`TakeSnapshot` captures the full `versions` map and `latest` map.
`RestoreSnapshot` replaces both atomically (same pattern as current
`KVStateMachine.RestoreSnapshot`).

Version history is preserved across snapshots (within retention window).

### Garbage collection

After each apply, if `len(versions[key]) > retention`:

```
versions[key] = versions[key][len(versions[key])-retention:]
```

Oldest entries are dropped first. `retention=0` disables GC (test mode).
Default: `retention=10` (configurable via `CORE_X_MVCC_RETENTION`).

---

## Invariants

| ID | Invariant |
|---|---|
| INV-MV1 | `latest[key]` always equals `versions[key][last].Version` |
| INV-MV2 | Version numbers are strictly monotonic per key across restarts |
| INV-MV3 | A CAS conflict does NOT advance `lastApplied`; `notifyWaiters` is still called so HTTP handlers are not blocked |
| INV-MV4 | Unconditional writes (`expected_version==0`) never conflict |
| INV-MV5 | Snapshot reads (`?version=N`) bypass ReadIndex; they are not linearizable but are repeatable |

---

## Trade-offs

| Factor | Impact |
|---|---|
| **Memory** | O(retention × keys) additional memory per node. At retention=10 and 1M keys, ~10M extra version entries. Acceptable for learning scope. |
| **Backward compatibility** | `expected_version==0` path is identical to current behaviour. No existing client breaks. |
| **Throughput** | CAS conflicts cause client retries but do not block the Raft pipeline. Failed CAS entries are committed (as no-ops) and do not stall replication. |
| **Snapshot isolation** | `?version=N` reads provide snapshot isolation at the cost of potentially serving old data. Documented in API contract. |

---

## What this teaches (DDIA mapping)

| Concept | DDIA reference | Core-X expression |
|---|---|---|
| Lost Update | Ch.7 §"Preventing Lost Updates" | CAS on stock counter example |
| Optimistic Locking | Ch.7 §"Compare-and-set" | `expected_version` field |
| MVCC | Ch.7 §"Snapshot Isolation and Repeatable Read" | `versions[]` per key |
| Version GC (Vacuum) | Ch.7 §"MVCC and Indexes" | retention-based trim |
| Snapshot read | Ch.7 §"Snapshot Isolation" | `GET /kv/{key}?version=N` |

---

## Implementation plan

| Step | File | Change |
|---|---|---|
| 1 | `raft/kv_state_machine.go` | Add `MVCCVersion`, `MVCCStateMachine`; keep `KVStateMachine` as alias or replace |
| 2 | `raft/kv_state_machine.go` | CAS apply logic + `conflictStore` |
| 3 | `http/unified_kv_handler.go` | `GET /kv/{key}?version=N` snapshot read |
| 4 | `http/handler.go` | `PUT /kv/{key}` CAS endpoint |
| 5 | Tests | CAS conflict scenario, lost update prevention, snapshot read |
