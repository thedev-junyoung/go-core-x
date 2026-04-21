# ADR-020: Joint Consensus for Dynamic Cluster Membership

**Status:** Accepted
**Date:** 2026-04-20
**Deciders:** junyoung
**Related:** ADR-010 (Raft Leader Election), ADR-013 (Raft Write Path), ADR-019 (Linearizable Read)

---

## Context

### Current Membership Model and Its Correctness Gap

`NewRaftNode(id, peers, meta, logStore)` receives `*PeerClients` at construction
time. `PeerClients` wraps a `map[string]*RaftClient` keyed by gRPC address,
which is populated once and never mutated:

```go
// internal/infrastructure/raft/node.go
type RaftNode struct {
    id    string
    peers *PeerClients // fixed at construction; nil in single-node mode
    ...
}
```

Quorum size is derived implicitly throughout the codebase as
`len(peers) + 1` (total) → `total/2 + 1` (majority). For example:

```go
// runCandidate
total    := len(peers) + 1
majority := total/2 + 1

// sendHeartbeats
total    := len(peers) + 1
majority := total/2 + 1
```

This fixed-peer model means **cluster membership cannot change at runtime**.
Any operator attempt to add or remove a node requires a full cluster restart with
new `CORE_X_PEERS` values — a window during which the cluster is unavailable.

### The Split-Brain Risk of Naive Membership Change

Suppose an operator wants to grow from 3 nodes [A, B, C] to 5 nodes [A, B, C, D, E].
If the membership update is applied by restarting nodes one by one with the new
peer list, there exists a transient window where two independent majorities can form:

```
Old config C_old = {A, B, C}  → quorum = 2  (A+B)
New config C_new = {A, B, C, D, E} → quorum = 3

Transition window (some nodes restarted, others not):
  A, B see C_old → A+B form quorum → elect A as leader
  D, E see C_new → A+D+E form quorum → elect D as leader
  → Two leaders in the same term → split-brain
```

This violates **Safety** (Raft §6): at most one leader may be elected per term.

The problem arises because old and new configurations are activated
independently on each node, rather than as a single coordinated transition.

### DDIA Framing

Ongaro §6 ("Cluster Membership Changes") directly addresses this. The three
DDIA pillars map as follows:

| Pillar | Risk | Mitigation |
|---|---|---|
| **Reliability** | Split-brain during transition → two leaders commit conflicting entries | Joint consensus: both quorums required simultaneously |
| **Scalability** | Membership changes are required for horizontal scaling | Online (no-restart) membership change via Raft log entries |
| **Maintainability** | Operator must manually coordinate restarts | `POST /raft/config` API; transition is automatic and observable |

---

## Decision

Implement **Joint Consensus** (Ongaro PhD thesis §6) as the membership change
protocol for Core-X. The transition proceeds in two phases, each driven by a
Raft log entry of a new type `EntryTypeConfig`:

1. **Phase A — C_old,new entry:** The leader proposes a joint configuration
   containing both the old and new membership sets. Once this entry is
   *appended to the leader's log* (not yet committed), the joint configuration
   takes effect immediately on the leader. Quorum for any decision requires
   both `majority(C_old)` AND `majority(C_new)`.

2. **Phase B — C_new entry:** Once C_old,new is committed (acknowledged by a
   joint quorum), the leader proposes a C_new entry. Once this entry is
   appended, the new configuration takes effect. Old members not in C_new can
   now shut down.

The key invariant from Raft §6: **no server ever switches directly from
C_old to C_new**; every server passes through C_old,new. Because C_old,new
requires a majority of *both* old and new quorums, it is impossible for a
member of C_old alone or C_new alone to form an independent majority.

---

## Design

### 1. `ClusterConfig` — Membership Representation

```go
// internal/infrastructure/raft/config.go

// ConfigPhase describes which phase of membership the cluster is in.
type ConfigPhase int

const (
    // ConfigPhaseStable: single active configuration (C_old or C_new).
    ConfigPhaseStable ConfigPhase = iota
    // ConfigPhaseJoint: transitional configuration C_old,new.
    // Decisions require majority(voters) AND majority(newVoters).
    ConfigPhaseJoint
)

// ClusterConfig represents the active cluster membership.
// In ConfigPhaseStable, only Voters is populated.
// In ConfigPhaseJoint, both Voters (C_old) and NewVoters (C_new) are populated.
type ClusterConfig struct {
    Phase     ConfigPhase
    Voters    []string // C_old peer addresses (always present)
    NewVoters []string // C_new peer addresses (non-nil during joint phase)
}

// IsJoint reports whether the cluster is in the joint-consensus phase.
func (c ClusterConfig) IsJoint() bool {
    return c.Phase == ConfigPhaseJoint
}

// OldQuorumSize returns the majority threshold for C_old (self included).
func (c ClusterConfig) OldQuorumSize() int {
    return len(c.Voters)/2 + 1
}

// NewQuorumSize returns the majority threshold for C_new (self included).
func (c ClusterConfig) NewQuorumSize() int {
    return len(c.NewVoters)/2 + 1
}
```

`ClusterConfig` is stored on `RaftNode` and protected by `n.mu`.
It is the single source of truth for all quorum calculations.

### 2. Log Entry Type — `EntryTypeConfig`

The WAL payload format defined in `log_store.go` uses a leading type byte.
A third type byte is added for config-change entries:

```go
// internal/infrastructure/raft/log_store.go

const (
    logRecordTypeEntry    = byte(0x01) // Raft log entry (data command)
    logRecordTypeTruncate = byte(0x02) // truncate-suffix marker
    // logRecordTypeConfig is persisted the same as logRecordTypeEntry;
    // the distinction is in LogEntry.EntryType, not the WAL record type.
    // WAL serialisation is unchanged.
)

// EntryType distinguishes regular commands from config-change entries.
type EntryType uint8

const (
    EntryTypeCommand EntryType = 0 // default: KV set/del command
    EntryTypeConfig  EntryType = 1 // cluster membership change
)

// LogEntry gains an EntryType field (zero-value = EntryTypeCommand, backward compatible).
type LogEntry struct {
    Index     int64
    Term      int64
    Type      EntryType // new field; zero value preserves existing behaviour
    Data      []byte
}
```

**Backward compatibility:** `Type == 0` (zero value) means `EntryTypeCommand`.
All existing log entries on disk have no type byte in `Data`; their `Type` field
remains 0 after WAL replay. No WAL format migration is required.

**WAL serialisation change:** `encodeLogEntry` gains a 1-byte `Type` field after
the existing header:

```
typeEntry [0x01] [Index:8 LE] [Term:8 LE] [EntryType:1] [DataLen:4 LE] [Data:N]
```

To preserve backward compatibility during LoadAll, if the decoded `EntryType`
byte equals 0 or 1 (both valid) it is used directly; payloads written by older
versions begin with the DataLen bytes and will have `EntryType = 0` only when
the DataLen high byte happens to be 0. To avoid ambiguity, a magic version byte
`0xFF` is prepended to all new WAL records:

> **Simpler alternative (chosen):** Store the config payload in `LogEntry.Data`
> as JSON, and keep `EntryType` in `LogEntry.Type` only in memory. WAL
> serialisation appends a 1-byte entry type *before* DataLen. Old readers that
> do not know about the type byte will misparse the DataLen — but since old
> binaries cannot run a cluster with config entries, this is acceptable.
> The WAL format is versioned at the record level by the leading `0x01` type
> byte already present; adding `EntryType` is an in-band extension.

**Config entry payload (JSON, stored in `LogEntry.Data`):**

```go
// ConfigChangePayload is the Data field of an EntryTypeConfig log entry.
type ConfigChangePayload struct {
    // Phase is the target phase after this entry is applied.
    // "joint"  → this entry transitions to C_old,new.
    // "stable" → this entry finalises C_new.
    Phase    string   `json:"phase"`
    Voters   []string `json:"voters"`    // complete C_old or C_new voter list
    NewVoters []string `json:"new_voters,omitempty"` // non-empty during joint phase
}
```

### 3. Quorum Calculation — `quorumFor()`

All quorum checks are centralised in a new method:

```go
// quorumFor returns true if the set of nodes that have acknowledged
// (via matchIndex ≥ N or vote count) satisfies the active quorum rule.
//
// ConfigPhaseStable: simple majority of Voters.
// ConfigPhaseJoint:  majority of Voters AND majority of NewVoters.
//
// Must be called with mu held.
func (n *RaftNode) quorumFor(cfg ClusterConfig, ackFn func(addr string) bool) bool {
    switch cfg.Phase {
    case ConfigPhaseStable:
        count := 1 // self
        for _, addr := range cfg.Voters {
            if ackFn(addr) {
                count++
            }
        }
        return count >= cfg.OldQuorumSize()

    case ConfigPhaseJoint:
        // Old quorum.
        oldCount := 1
        for _, addr := range cfg.Voters {
            if ackFn(addr) {
                oldCount++
            }
        }
        // New quorum.
        newCount := 1
        for _, addr := range cfg.NewVoters {
            if ackFn(addr) {
                newCount++
            }
        }
        return oldCount >= cfg.OldQuorumSize() && newCount >= cfg.NewQuorumSize()
    }
    return false
}
```

`maybeAdvanceCommitIndex` and `runCandidate` are updated to call `quorumFor`
instead of computing `majority` inline. The self-vote is always counted (the
leader is always in both C_old and C_new; a candidate campaigns in both).

### 4. Config Entry Application — "Immediately on Append"

Raft §6 mandates that a config change entry takes effect as soon as it is
appended to the log — not when it is committed. This is what prevents
split-brain: the leader enforces the joint quorum rule from the moment it
writes the C_old,new entry, before any follower has acknowledged it.

```go
// applyConfigEntry updates n.clusterConfig from a ConfigChangePayload.
// Called immediately after appending an EntryTypeConfig entry to n.log.
// Must be called with mu held.
func (n *RaftNode) applyConfigEntry(entry LogEntry) {
    var payload ConfigChangePayload
    if err := json.Unmarshal(entry.Data, &payload); err != nil {
        slog.Error("raft: malformed config entry", "index", entry.Index, "err", err)
        return
    }
    switch payload.Phase {
    case "joint":
        n.clusterConfig = ClusterConfig{
            Phase:     ConfigPhaseJoint,
            Voters:    payload.Voters,
            NewVoters: payload.NewVoters,
        }
        slog.Info("raft: joint config active",
            "old", payload.Voters, "new", payload.NewVoters)
    case "stable":
        n.clusterConfig = ClusterConfig{
            Phase:  ConfigPhaseStable,
            Voters: payload.Voters,
        }
        slog.Info("raft: stable config active", "voters", payload.Voters)
    }
    // Expand PeerClients to include any new voters not already connected.
    n.peers.EnsureConnected(allVoters(n.clusterConfig))
}
```

`applyConfigEntry` is invoked in two places:

1. In `Propose` (leader path) — immediately after `n.log = append(n.log, entry)`.
2. In `HandleAppendEntries` (follower path) — immediately after the entry is
   appended to `n.log` (before the loop continues to the next entry).

### 5. Leader Config-Change Orchestration

The leader drives the two-phase protocol automatically:

```go
// ProposeConfigChange initiates a membership change.
// It proposes C_old,new and, once committed, automatically proposes C_new.
//
// Callers (e.g. ConfigHandler.ServeHTTP) call this method and wait for the
// returned channel to receive the final applied index.
//
// Must NOT be called with mu held.
func (n *RaftNode) ProposeConfigChange(ctx context.Context, newVoters []string) (<-chan int64, error) {
    n.mu.Lock()
    if n.role != RoleLeader {
        n.mu.Unlock()
        return nil, ErrNotLeader
    }
    if n.clusterConfig.IsJoint() {
        n.mu.Unlock()
        return nil, ErrConfigChangeInProgress
    }
    oldVoters := n.clusterConfig.Voters // snapshot C_old

    // Phase A: propose C_old,new.
    jointPayload := ConfigChangePayload{
        Phase:     "joint",
        Voters:    oldVoters,
        NewVoters: newVoters,
    }
    data, _ := json.Marshal(jointPayload)
    jointEntry := LogEntry{Type: EntryTypeConfig, Data: data}

    jointIndex, _, isLeader := n.proposeEntry(jointEntry) // internal, mu held
    n.mu.Unlock()

    if !isLeader {
        return nil, ErrNotLeader
    }

    done := make(chan int64, 1)

    // Background goroutine: wait for C_old,new to commit, then propose C_new.
    go func() {
        // Wait for joint entry to commit.
        waitCtx, cancel := context.WithTimeout(ctx, configChangeTimeout)
        defer cancel()

        if err := n.waitForCommit(waitCtx, jointIndex); err != nil {
            slog.Error("raft: joint config commit timed out", "err", err)
            close(done)
            return
        }

        // Phase B: propose C_new.
        n.mu.Lock()
        if n.role != RoleLeader {
            n.mu.Unlock()
            close(done)
            return
        }
        stablePayload := ConfigChangePayload{
            Phase:  "stable",
            Voters: newVoters,
        }
        data, _ := json.Marshal(stablePayload)
        stableEntry := LogEntry{Type: EntryTypeConfig, Data: data}
        stableIndex, _, isLeader := n.proposeEntry(stableEntry)
        n.mu.Unlock()

        if !isLeader {
            close(done)
            return
        }

        waitCtx2, cancel2 := context.WithTimeout(ctx, configChangeTimeout)
        defer cancel2()
        if err := n.waitForCommit(waitCtx2, stableIndex); err != nil {
            slog.Error("raft: stable config commit timed out", "err", err)
            close(done)
            return
        }
        done <- stableIndex
    }()

    return done, nil
}
```

`configChangeTimeout = 30 * time.Second` (configurable via env var in a later phase).

### 6. HTTP API — `POST /raft/config`

```go
// internal/infrastructure/http/config_handler.go

type configChangeRequest struct {
    Action string   `json:"action"` // "add" or "remove"
    Nodes  []string `json:"nodes"`  // peer gRPC addresses to add or remove
}

type ConfigHandler struct {
    node    *infraraft.RaftNode
    addrMap map[string]string
}

// ServeHTTP handles POST /raft/config.
//
// Flow:
//  1. Decode JSON body → action + node list
//  2. Compute new voter list from current config + delta
//  3. node.ProposeConfigChange(ctx, newVoters) → doneCh
//  4. Wait for doneCh (30s timeout)
//  5. 204 No Content on success
func (h *ConfigHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    var req configChangeRequest
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
        http.Error(w, "invalid JSON body", http.StatusBadRequest)
        return
    }
    if req.Action != "add" && req.Action != "remove" {
        http.Error(w, `"action" must be "add" or "remove"`, http.StatusBadRequest)
        return
    }
    if len(req.Nodes) == 0 {
        http.Error(w, `"nodes" must not be empty`, http.StatusBadRequest)
        return
    }

    // Compute new voter set.
    newVoters, err := h.node.ComputeNewVoters(req.Action, req.Nodes)
    if err != nil {
        http.Error(w, err.Error(), http.StatusConflict)
        return
    }

    ctx, cancel := context.WithTimeout(r.Context(), 35*time.Second)
    defer cancel()

    doneCh, err := h.node.ProposeConfigChange(ctx, newVoters)
    if err != nil {
        if errors.Is(err, infraraft.ErrNotLeader) {
            if leaderID := h.node.LeaderID(); leaderID != "" {
                if baseURL, ok := h.addrMap[leaderID]; ok {
                    http.Redirect(w, r, baseURL+"/raft/config", http.StatusTemporaryRedirect)
                    return
                }
            }
            http.Error(w, "not the Raft leader", http.StatusServiceUnavailable)
            return
        }
        if errors.Is(err, infraraft.ErrConfigChangeInProgress) {
            http.Error(w, "config change already in progress", http.StatusConflict)
            return
        }
        http.Error(w, "internal error", http.StatusInternalServerError)
        return
    }

    select {
    case appliedIndex, ok := <-doneCh:
        if !ok {
            http.Error(w, "config change failed", http.StatusInternalServerError)
            return
        }
        w.Header().Set("X-Raft-Applied-Index", strconv.FormatInt(appliedIndex, 10))
        w.WriteHeader(http.StatusNoContent)
    case <-ctx.Done():
        http.Error(w, "config change timed out", http.StatusGatewayTimeout)
    }
}
```

Registered in `cmd/main.go` alongside existing Raft routes:

```go
if raftNode != nil {
    mux.Handle("POST /raft/config", infrahttp.NewConfigHandler(raftNode, raftAddrMap))
}
```

### 7. `PeerClients.EnsureConnected`

New nodes that join via config change need gRPC connections established.
`EnsureConnected` adds connections for addresses not already present:

```go
// EnsureConnected dials any addresses in addrs that are not yet in n.clients.
// Safe for concurrent use.
func (p *PeerClients) EnsureConnected(addrs []string) {
    p.mu.Lock()
    defer p.mu.Unlock()
    for _, addr := range addrs {
        if _, ok := p.clients[addr]; !ok {
            conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
            if err != nil {
                slog.Warn("raft: failed to connect to new peer", "addr", addr, "err", err)
                continue
            }
            p.clients[addr] = &RaftClient{addr: addr, conn: conn, ...}
        }
    }
}
```

---

## Safety Analysis (DDIA Chapter 9 / Raft §6)

### Invariant 1 — No Two Leaders in the Same Term (Election Safety)

During joint consensus, a candidate must win a majority in *both* C_old and
C_new to be elected. Proof by contradiction:

- Assume two candidates X and Y are elected in the same term T.
- X won `majority(C_old)` ∩ `majority(C_new)`.
- Y won `majority(C_old)` ∩ `majority(C_new)`.
- Any two majorities within the *same* set share at least one member
  (pigeonhole principle). A voter grants at most one vote per term (§5.2).
- Therefore X and Y cannot both have won; contradiction. QED.

This holds for the stable phases (C_old only, C_new only) by the original Raft
safety proof.

### Invariant 2 — No Config Skips (Every Node Passes Through C_old,new)

- The C_old,new entry is replicated via `AppendEntries` before any node can
  act on C_new.
- A node applies a config entry *on append*, not on commit. This means:
  - The leader transitions to joint quorum immediately after proposing C_old,new.
  - A follower transitions to joint quorum as soon as it receives and appends the entry.
  - A new leader elected after C_old,new is appended (but before committed) will
    have the C_old,new entry in its log (election safety §5.4.1: leader
    completeness). It will continue to enforce joint quorum.

### Invariant 3 — Leader Crash After C_old,new (Key Edge Case)

If the leader crashes after proposing C_old,new but before it commits:

- **Sub-case A: C_old,new not replicated to any peer.** The entry is only in
  the crashed leader's log. After re-election, the new leader's log does not
  contain C_old,new (it was not replicated). The new leader operates in C_old.
  The operator must retry the config change. No split-brain.

- **Sub-case B: C_old,new replicated to a quorum.** By Raft log completeness
  (§5.4.1), the new leader will have C_old,new in its log. It applies C_old,new
  on startup (log replay), transitions to joint quorum, and continues Phase B
  automatically once it commits the entry. No split-brain.

In both sub-cases, the protocol converges safely. The HTTP response to the
operator may time out; the operator retries `POST /raft/config`.

### Invariant 4 — New Node Needs Snapshot

A joining node D may be so far behind that its log does not contain all
committed entries. The leader detects this via `nextIndex[D] <= snapshotIndex`
and triggers `sendSnapshot` (existing Phase 9 mechanism). D receives the
snapshot, restores state, and begins receiving `AppendEntries` normally. D does
not vote until it is included in C_new; during the joint phase it only receives
log entries (it is in `NewVoters`, so its acks count toward `majority(C_new)`).

### Invariant 5 — Removed Nodes

A node removed from C_new learns of its removal when the C_new entry is
committed and applied. At that point its quorum membership is 0; it receives no
more AppendEntries from the leader and will time out, start elections, and
collect 0 votes (no current member will vote for it — they are already in
C_new). It may be shut down by the operator at any time after removal.

To prevent a removed node from disrupting the cluster indefinitely (it will
keep incrementing its term and sending RequestVote), the leader should ignore
RequestVote from non-members (addresses not in its current Voters list). This
is a defence-in-depth measure; it does not affect safety.

---

## Trade-off Analysis

| Dimension | Choice | Benefit | Cost |
|---|---|---|---|
| **Correctness** | Joint consensus (2-phase) | No window where two quorums exist; guaranteed by Raft §6 proof | Two Raft log entries per membership change vs. one for single-server change |
| **Complexity** | Centralised `quorumFor()` | All quorum logic in one function; easy to audit | New `ClusterConfig` type leaks into `RaftNode` struct and all callers |
| **Availability** | Config entry takes effect on append, not commit | Leader enforces joint quorum immediately; no unsafe window | If leader crashes after append but before commit (sub-case B), new leader must replay and continue — requires idempotent apply logic |
| **Backward compat** | `EntryType` zero-value = command | No WAL migration needed | WAL format changes silently if an old binary reads a new WAL — must document |
| **Atomicity** | Two-phase via goroutine in leader | Simple implementation; no new timer goroutine | If the leader steps down between Phase A commit and Phase B propose, Phase B is abandoned; operator must retry |
| **New node bootstrap** | Reuse existing `sendSnapshot` path | No new mechanism for lagging joiners | Snapshot must include current `ClusterConfig`; snapshot format (ADR-017) needs a `Membership` field |

### When Does This Trade-off Break?

- **High churn:** If membership changes happen faster than two Raft round-trips
  (2 × network RTT × quorum size), `ErrConfigChangeInProgress` will be returned
  repeatedly. Mitigation: serialise changes via a queue (not implemented in v2).
- **Long joint phase:** If Phase B stalls (leader partition after Phase A
  commit), the cluster operates under the more restrictive joint quorum
  indefinitely. Old members cannot shut down safely. Mitigation: operator
  alerting on `raft_config_phase=joint` metric for > N minutes.
- **Snapshot membership mismatch:** If `ClusterConfig` is not persisted in
  snapshots, a node recovering from snapshot will revert to initial static
  config. Mitigation: include `ClusterConfig` in `SnapshotMeta` (Phase 12 scope).

---

## Implementation Plan

### Step 1 — `ClusterConfig` and `EntryType` (internal/infrastructure/raft)

**Files modified:**
- `internal/infrastructure/raft/config.go` ← new file
- `internal/infrastructure/raft/log_store.go` — add `EntryType` to `LogEntry`; update encode/decode
- `internal/infrastructure/raft/state.go` — add `ErrConfigChangeInProgress` sentinel

**Scope:** data types only; no behaviour change. All existing tests must pass.

### Step 2 — `RaftNode` gets `clusterConfig` field and `quorumFor()`

**Files modified:**
- `internal/infrastructure/raft/node.go`:
  - Add `clusterConfig ClusterConfig` field to `RaftNode`
  - `NewRaftNode`: initialise `clusterConfig` from `peers.Addrs()` (stable, C_old = all peer addrs + self)
  - Replace inline `majority := total/2 + 1` in `runCandidate` and `sendHeartbeats`
    with `quorumFor()` calls
  - Add `applyConfigEntry()` called from `Propose` and `HandleAppendEntries`
  - Add `ProposeConfigChange()` and `ComputeNewVoters()`

**Scope:** quorum logic change. Existing 3-node chaos tests must still pass
(stable config path is structurally identical to current logic).

### Step 3 — `PeerClients.EnsureConnected`

**Files modified:**
- `internal/infrastructure/raft/peers.go` — add `EnsureConnected(addrs []string)`

**Scope:** gRPC connection management only.

### Step 4 — HTTP handler `POST /raft/config`

**Files modified:**
- `internal/infrastructure/http/config_handler.go` ← new file
- `cmd/main.go` — register `POST /raft/config` route

**Scope:** HTTP layer only; no Raft core changes.

### Step 5 — Integration Tests

**Files created:**
- `internal/infrastructure/raft/joint_consensus_test.go`

**Test scenarios (in-process, OS-process-isolation where needed):**

| Scenario | Assertion |
|---|---|
| 3→5 expansion (happy path) | C_old,new committed, C_new committed, 5-node quorum forms, no data loss |
| 5→3 shrink | Removed nodes shut down; 3-node quorum operates; no split-brain |
| Leader crash after C_old,new appended, before committed | New leader re-applies C_old,new; Phase B completes; stable config reached |
| Leader crash after C_old,new committed, before C_new proposed | New leader detects joint config on startup; auto-proposes C_new |
| New node starts from snapshot (lagging joiner) | sendSnapshot path used; D joins C_new and participates in quorum |
| Config change during network partition | POST /raft/config times out; cluster remains in C_old; no split-brain |
| Duplicate config change request | `ErrConfigChangeInProgress` returned on second call; first change completes normally |

---

## Consequences

### What Changes

1. `RaftNode` gains a `clusterConfig ClusterConfig` field (protected by `n.mu`).
2. All quorum decisions route through `quorumFor()` — single audit point.
3. `LogEntry.Type` is a new field; zero-value preserves existing behaviour.
4. WAL serialisation gains 1 byte per entry (negligible overhead).
5. `POST /raft/config` endpoint is available in cluster mode.
6. `PeerClients` is no longer fully immutable after construction.

### What Does Not Change

1. The Raft core loop (`runFollower`, `runCandidate`, `runLeader`) structure
   is unchanged; only the quorum check call sites are updated.
2. WAL format is append-compatible; no migration needed for existing data.
3. Snapshot format (ADR-017) is unchanged in Phase 11; `ClusterConfig`
   persistence in snapshots is deferred to Phase 12 (Storage Unification).
4. `CORE_X_PEERS` static bootstrap is still used for initial cluster formation;
   dynamic membership change is an additional capability, not a replacement.

### Risks and Mitigations

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| Snapshot does not capture `ClusterConfig` → stale config after restore | Medium | High (wrong quorum after recovery) | Add `ClusterConfig` to `SnapshotMeta` in Phase 12; document as known gap |
| Leader steps down mid-change → Phase B never proposed | Medium | Medium (cluster stuck in joint quorum) | Detect `IsJoint()` on new leader startup; auto-propose C_new |
| Removed node disrupts cluster with RequestVote | Low | Low (disruption, not split-brain) | Leader ignores RequestVote from non-members |
| WAL replay applies config entries out of order | Very Low | High | `LoadAll` returns entries in index order; `applyConfigEntry` is called in index order during replay |

### Monitoring Signals

| Signal | Alert Condition |
|---|---|
| `raft_config_phase{phase="joint"}` gauge | > 0 for more than 60 seconds → Phase B stalled |
| `raft_config_change_total{result="error"}` counter | Rate > 0 sustained → operator retry loop or bug |
| `raft_cluster_size` gauge | Diverges from expected node count → membership not applied everywhere |
| `raft_commit_index` divergence across nodes | > 1000 entries → new joiner may need snapshot |

### Validation

The design is considered validated when:

1. All existing unit and chaos tests pass unchanged (stable quorum path not regressed).
2. 3→5 expansion test: no data loss, no leader conflict, C_new committed within 30s.
3. Leader-crash-during-joint test: new leader auto-completes Phase B.
4. `POST /raft/config` returns 204 with `X-Raft-Applied-Index` header on success.
5. `raft_config_phase` metric transitions: `stable` → `joint` → `stable`.
