# ADR-019: Linearizable Read via ReadIndex Protocol

**Status:** Accepted
**Date:** 2026-04-20
**Deciders:** junyoung
**Related:** ADR-014 (KV State Machine), ADR-015 (Leader Redirect), ADR-018 (Dual Write-Path)

---

## Context

### Current Read Path and Its Correctness Gap

`GET /raft/kv/{key}` is served by `RaftKVGetHandler.ServeHTTP`, which calls
`sm.Get(key)` — a plain `sync.RWMutex`-guarded map lookup with no leader
verification:

```go
// internal/infrastructure/http/propose_handler.go
func (h *RaftKVGetHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    key := r.PathValue("key")
    value, ok := h.sm.Get(key) // direct map read — no leader check
    ...
}
```

`KVStateMachine.Get` holds only `sm.mu.RLock()`:

```go
func (sm *KVStateMachine) Get(key string) (string, bool) {
    sm.mu.RLock()
    defer sm.mu.RUnlock()
    return sm.data[key], sm.data[key] != ""
}
```

This produces a **stale read** in the following scenario:

```
Timeline:
  T0: Node A is leader. Client writes key="x", value="2". Committed.
  T1: Network partitions: A is isolated from B and C.
  T2: B and C elect a new leader (B). Client writes key="x", value="3". Committed on {B,C}.
  T3: Client sends GET /raft/kv/x to A (stale leader).
  T4: A returns "2" ← stale, violates Linearizability.
```

Node A's `RaftNode.role` is still `RoleLeader` (no one told it otherwise), so
even a naive role check would not catch this scenario without a quorum
confirmation.

This violates **Linearizability** (Herlihy & Wing 1990): every read must
observe the effects of all writes that completed before the read began.

### DDIA Framing

Per DDIA Chapter 9 ("Consistency and Consensus"):

- **Reliability** dimension: stale reads cause client-visible data anomalies
  that are indistinguishable from data loss.
- **Scalability** dimension: reads are on the critical throughput path; the
  solution must not serialize all reads through a single bottleneck.
- **Maintainability** dimension: the fix must be operationally transparent and
  testable without modifying the consensus core.

---

## Decision

Adopt the **ReadIndex Protocol** (Raft §6.4, etcd implementation) as the
primary linearizable read mechanism. Implement **Lease Read** as an optional
fast path, gated behind the environment variable `CORE_X_RAFT_LEASE_READ=true`.

---

## ReadIndex Protocol Design

### Conceptual Guarantees

ReadIndex provides linearizability by ensuring that before serving a read, the
leader has:

1. Confirmed it is still the leader (via a quorum heartbeat round).
2. Identified its `commitIndex` at the moment the read was received.
3. Waited until `lastApplied >= commitIndex`.

This guarantees the read observes all writes committed up to the moment the
request arrived.

### `RaftNode.ReadIndex(ctx context.Context) (uint64, error)`

```go
// ReadIndex confirms leadership and returns the commitIndex that a linearizable
// read must wait for before serving data.
//
// Algorithm (Raft §6.4):
//  1. Reject immediately if not currently the leader.
//  2. Capture commitIndex under mu.
//  3. Broadcast a heartbeat AppendEntries (empty entries) to all peers.
//  4. If a quorum responds successfully before ctx expires → return capturedCommitIndex.
//  5. If fewer than quorum respond → return error (read must be retried or rejected).
//
// The caller must then call sm.WaitForIndex(ctx, readIndex) before reading
// sm.Get(), which ensures lastApplied >= readIndex.
func (n *RaftNode) ReadIndex(ctx context.Context) (uint64, error)
```

#### Internal Mechanics

```
Leader receives GET /raft/kv/x
│
├─ 1. n.mu.Lock()
│     role == RoleLeader?  No → return 0, ErrNotLeader
│     capturedCommitIndex = n.commitIndex
│  n.mu.Unlock()
│
├─ 2. Send empty AppendEntries to all peers (quorum heartbeat)
│     Reuse existing sendHeartbeats path with a readIndex-specific result channel.
│     No new log entries — purely a leadership confirmation round.
│
├─ 3. Collect responses:
│     success count (self always +1) >= majority → proceed
│     ctx expired or error count tips past minority → return ErrReadIndexTimeout
│
└─ 4. Return capturedCommitIndex
```

**Why an empty AppendEntries and not a separate RPC?**
Reusing the existing `AppendEntries` path (with zero entries) avoids new proto
definitions and test surface. Followers respond identically to a heartbeat.
The response term check in `sendHeartbeats` already handles step-down on
seeing a higher term, so the quorum heartbeat automatically handles the
partition scenario.

**Quorum heartbeat implementation approach:**

Rather than sharing `sendHeartbeats` (which drives the entire leader loop),
`ReadIndex` will use a dedicated internal method:

```go
// confirmLeadership sends a quorum heartbeat and returns true if a majority
// of peers acknowledged before ctx expires.
// Must NOT be called with mu held.
func (n *RaftNode) confirmLeadership(ctx context.Context, term int64) bool
```

This keeps the readIndex path independent from the background heartbeat ticker,
avoiding interference with the leader's commit pipeline.

### WaitForIndex Integration

`KVStateMachine.WaitForIndex` already exists and is correct:

```go
func (sm *KVStateMachine) WaitForIndex(ctx context.Context, index int64) error
```

The full linearizable read flow:

```go
// RaftKVGetHandler.ServeHTTP — proposed change
readIndex, err := h.node.ReadIndex(r.Context())
if errors.Is(err, raft.ErrNotLeader) {
    // redirect to known leader (reuse ADR-015 redirect logic)
    ...
    return
}
if err != nil {
    http.Error(w, "read index unavailable", http.StatusServiceUnavailable)
    return
}
if err := h.sm.WaitForIndex(r.Context(), int64(readIndex)); err != nil {
    http.Error(w, "timed out waiting for state machine", http.StatusGatewayTimeout)
    return
}
value, ok := h.sm.Get(key)
...
```

**Latency budget:** ReadIndex adds one network round-trip (heartbeat RTT,
typically 1–5 ms on LAN). The existing `WaitForIndex` path can add up to
`applyPollInterval` (5 ms) if `lastApplied` is lagging. Total overhead per
read: ~10 ms worst-case on LAN, dominated by heartbeat RTT.

---

## Lease Read Design

Lease Read eliminates the network round-trip by tracking the leader's lease
validity based on heartbeat timing. A leader that successfully received quorum
acknowledgments within `electionTimeout / clock_drift_bound` can serve reads
locally without a confirmation round.

### Safety Precondition

Lease Read is **only safe** if the following invariant holds:

> A leader's lease expires before any other node can win an election.

This requires: `leaseTimeout < ElectionTimeoutMin / max_clock_drift`

In Core-X: `ElectionTimeoutMin = 150ms`. Assuming ≤5% clock drift:
`leaseTimeout ≤ 142ms`. We use `130ms` as a conservative safe value.

### `leaseExpiry` Field on `RaftNode`

```go
// RaftNode additions:
type RaftNode struct {
    ...
    // leaseExpiry is the monotonic deadline until which this leader's lease
    // is valid. Zero value means no active lease.
    // Protected by mu.
    leaseExpiry time.Time
    // leaseEnabled is set at construction from CORE_X_RAFT_LEASE_READ=true.
    // Immutable after NewRaftNode — no lock needed for reads.
    leaseEnabled bool
}
```

### Lease Renewal

The leader renews its lease each time `sendHeartbeats` collects a quorum
acknowledgment. This reuses the existing quorum-counting logic in
`sendHeartbeats`:

```go
// After maybeAdvanceCommitIndex, if quorum was reached:
if quorumAcknowledged && n.leaseEnabled {
    n.leaseExpiry = time.Now().Add(leaseDuration) // leaseDuration = 130ms
}
```

`leaseDuration` will be a package-level constant:

```go
const leaseDuration = 130 * time.Millisecond
```

### `ReadIndex` with Lease Fast Path

```go
func (n *RaftNode) ReadIndex(ctx context.Context) (uint64, error) {
    n.mu.Lock()
    if n.role != RoleLeader {
        n.mu.Unlock()
        return 0, ErrNotLeader
    }
    capturedCommitIndex := uint64(n.commitIndex)

    // Lease Read fast path: if lease is valid, skip quorum heartbeat.
    if n.leaseEnabled && time.Now().Before(n.leaseExpiry) {
        n.mu.Unlock()
        return capturedCommitIndex, nil
    }
    term := n.currentTerm
    n.mu.Unlock()

    // ReadIndex path: confirm leadership via quorum heartbeat.
    if !n.confirmLeadership(ctx, term) {
        return 0, ErrReadIndexTimeout
    }
    return capturedCommitIndex, nil
}
```

### Lease Read Risk Surface

| Risk | Mitigation |
|---|---|
| Clock skew causes expired lease to appear valid | Conservative `leaseDuration` (87% of ElectionTimeoutMin); document requirement for NTP sync |
| VM pause / GC pause exceeds lease duration | Cannot fully mitigate without hardware clocks; documented as known limitation |
| `leaseEnabled=true` on follower (misconfiguration) | `ReadIndex` checks `role == RoleLeader` first; followers return `ErrNotLeader` |

**Recommendation:** keep `CORE_X_RAFT_LEASE_READ=false` (default) in
production until clock discipline is verified. Lease Read is a performance
optimization, not a correctness requirement.

---

## GET Handler Change Plan

`RaftKVGetHandler` needs a reference to `RaftNode` to call `ReadIndex`.
Currently it only holds `*KVStateMachine`.

### Structural Change

```go
// Before:
type RaftKVGetHandler struct {
    sm *infraraft.KVStateMachine
}

// After:
type RaftKVGetHandler struct {
    node    *infraraft.RaftNode
    sm      *infraraft.KVStateMachine
    addrMap map[string]string // for leader redirect (reuse ADR-015 pattern)
}
```

### Updated ServeHTTP Flow

```
GET /raft/kv/{key}
│
├─ node.ReadIndex(ctx)
│   ├─ ErrNotLeader → 307 redirect to leader (addrMap lookup)
│   └─ ErrReadIndexTimeout → 503
│
├─ sm.WaitForIndex(ctx, readIndex)
│   └─ timeout → 504
│
└─ sm.Get(key)
    ├─ found → 200 JSON
    └─ not found → 404
```

---

## Consequences

### Trade-offs

| Dimension | ReadIndex | Lease Read |
|---|---|---|
| **Correctness** | Linearizable (unconditional) | Linearizable (requires clock discipline) |
| **Read latency** | +1 heartbeat RTT (~2–5 ms LAN) | +0 ms (local check only) |
| **Throughput** | Each read triggers a heartbeat; at high QPS this batches naturally | No additional network traffic |
| **Failure behavior** | Reads fail fast when leader is isolated (correct: no stale data) | Reads continue during the lease window even after isolation (brief stale risk if clocks drift) |
| **Operational complexity** | None (no configuration required) | Requires NTP discipline and clock drift bounds documentation |

### What We're Optimizing For

**Primary: Correctness (Reliability pillar).** A stale read that looks like
committed data is indistinguishable from data corruption at the application
level. Linearizability is non-negotiable for a consensus-based store.

**Secondary: Low operational complexity (Maintainability pillar).** ReadIndex
requires no configuration, no clock assumptions, and produces deterministic
failure modes (timeout → 503/504, not silently wrong data).

### What We're Sacrificing

Each `GET /raft/kv/{key}` now costs one heartbeat round-trip. At 1000 reads/s
with 3 ms LAN RTT, this adds 3 seconds of aggregate network time per second —
sustainable with goroutine-per-request given the existing worker model, but
notable at very high read QPS. Lease Read mitigates this at the cost of clock
risk.

### When This Design Fails

| Condition | Failure Mode | Client Experience |
|---|---|---|
| Network partition isolates leader | `confirmLeadership` times out | 503 Service Unavailable — correct, no stale data |
| Quorum heartbeat takes > ctx deadline | `ErrReadIndexTimeout` | 503 or 504 — correct |
| Lease Read + clock skew > `leaseDuration` | Brief stale window (~ms) | Silent — this is the documented risk of enabling lease read |
| All nodes unreachable | Every read fails | 503 — expected in full partition |

---

## Implementation Plan

### Phase Order

**Step 1 — `RaftNode.ReadIndex` + `confirmLeadership`**
File: `internal/infrastructure/raft/node.go`

- Add `ErrNotLeader`, `ErrReadIndexTimeout` sentinel errors to `state.go`.
- Implement `confirmLeadership(ctx, term) bool` as a focused internal method.
- Implement `ReadIndex(ctx) (uint64, error)` using `confirmLeadership`.
- No `leaseExpiry` yet — add it only in Step 3.

**Step 2 — `RaftKVGetHandler` wired to `ReadIndex`**
File: `internal/infrastructure/http/propose_handler.go`

- Extend `RaftKVGetHandler` with `node *infraraft.RaftNode` and `addrMap`.
- Update `NewRaftKVGetHandler` constructor accordingly.
- Update `ServeHTTP` to call `ReadIndex → WaitForIndex → Get`.
- Update `cmd/main.go` wiring.

**Step 3 — Lease Read**
File: `internal/infrastructure/raft/node.go`

- Add `leaseExpiry time.Time` and `leaseEnabled bool` to `RaftNode`.
- Read `CORE_X_RAFT_LEASE_READ` env var in `NewRaftNode`.
- Renew `leaseExpiry` inside `sendHeartbeats` on quorum acknowledgment.
- Add lease fast-path branch to `ReadIndex`.

**Step 4 — Integration tests**
File: `internal/infrastructure/raft/linearizable_read_test.go` (new)

- Test: write via `Propose`, immediately read via `ReadIndex + WaitForIndex + Get` → must return new value.
- Test: simulate partition (cancel peer contexts), verify `ReadIndex` returns error rather than stale data.
- Test: lease read enabled, lease not expired → no heartbeat sent, correct value returned.
- Test: lease read enabled, lease expired → falls back to ReadIndex heartbeat.

**Step 5 — Chaos test extension**
File: `tools/chaos/` (existing chaos harness)

- Add linearizable read scenario: concurrent writes and reads under partition;
  assert no client reads a value that was never written or predates a confirmed write.

### Files Modified Per Step

| Step | Files Modified |
|---|---|
| 1 | `internal/infrastructure/raft/state.go`, `internal/infrastructure/raft/node.go` |
| 2 | `internal/infrastructure/http/propose_handler.go`, `cmd/main.go` |
| 3 | `internal/infrastructure/raft/node.go` |
| 4 | `internal/infrastructure/raft/linearizable_read_test.go` (new) |
| 5 | `tools/chaos/` (extend existing) |

---

## Monitoring / Validation

| Signal | Source | Alert Condition |
|---|---|---|
| `raft_read_index_duration_ms` histogram | `ReadIndex` method | p99 > 20 ms (2× expected LAN RTT) |
| `raft_read_index_errors_total` counter | `ReadIndex` error returns | > 0 sustained for > 10 s |
| `raft_lease_renewals_total` counter | `sendHeartbeats` quorum path | Drops to 0 while leader is healthy → clock/timing issue |
| `raft_stale_read_guard_triggered_total` | `WaitForIndex` path | Non-zero confirms apply lag is occurring |

**Correctness validation procedure:**

1. Start 3-node cluster.
2. Write `key=x`, `value=v1` via POST /raft/kv. Confirm 204.
3. Partition the leader from all peers (iptables or kill peer gRPC listeners).
4. Immediately send GET /raft/kv/x to the isolated leader.
5. **Expected:** 503 Service Unavailable (ReadIndex timeout), NOT `{"value":"v1"}`.
6. Restore partition. Confirm cluster re-elects and reads are served again.

---

## References

- Ongaro, D. (2014). *Consensus: Bridging Theory and Practice*. §6.4 Processing read-only queries.
- etcd: `etcdserver/raft.go` — ReadIndex implementation reference.
- Kleppmann, M. (2017). *Designing Data-Intensive Applications*. Chapter 9: Consistency and Consensus.
- ADR-014: Raft KV State Machine (`WaitForIndex` design).
- ADR-015: Leader Redirect pattern (reused for `ErrNotLeader` handling in GET handler).
