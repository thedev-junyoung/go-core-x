# ADR-018: Intentional Dual Write-Path Architecture (Ingestion WAL vs Raft KV)

**Status:** Accepted
**Date:** 2026-04-16
**Deciders:** junyoung
**Related:** ADR-005 (Bitcask KV), ADR-007 (Consistent Hashing), ADR-013 (Raft Write Path), ADR-014 (KV State Machine)

---

## Context

Core-X exposes two distinct write paths that both accept key-value data:

| Endpoint | Path | Storage Backend |
|---|---|---|
| `POST /ingest` | Consistent Hashing → gRPC forward → WAL | Bitcask KV Store (per-shard) |
| `POST /raft/kv` | Raft consensus → KVStateMachine | In-memory map snapshotted to disk |

A reviewer examining the codebase for the first time may conclude this is a design defect: why does the system have two separate storage layers for essentially the same operation?

This ADR documents that the coexistence is **intentional**, explains the distinct learning objectives each path serves, and establishes the boundary rules that prevent the two paths from corrupting each other's invariants.

---

## Decision

The dual-path architecture is **preserved as-is for v1**. No unification is planned until v2 defines a concrete migration story.

### Why two paths exist

**The ingestion path (`/ingest` → Bitcask) was built first** (Phase 1–3, ADR-001 through ADR-007). Its purpose was to study:
- Append-only WAL design and zero-allocation I/O (ADR-004)
- In-memory hash index with O(1) read (ADR-005)
- WAL compaction and tombstone GC (ADR-006)
- Consistent hashing for horizontal partitioning (ADR-007)

These are *single-node durability* and *stateless partitioning* concepts from DDIA Chapters 3 and 6.

**The Raft path (`/raft/kv` → KVStateMachine) was built later** (Phase 5–9, ADR-010 through ADR-017). Its purpose was to study:
- Leader election and log replication (ADR-010–ADR-013)
- Consensus-backed state machine (ADR-014)
- Snapshotting and log compaction via InstallSnapshot (ADR-017)

These are *distributed consensus* and *fault tolerance* concepts from DDIA Chapters 7, 8, and 9.

The two paths are **studying different chapters of the same book**. Merging them prematurely would obscure the architectural boundaries that each chapter's concepts depend on.

### Path isolation invariants

To prevent interference between the two paths:

1. **No shared storage:** The Bitcask KV store (`internal/infrastructure/storage/kv/`) and `KVStateMachine` (`internal/infrastructure/raft/kv_state_machine.go`) are separate in-memory indexes backed by separate files on disk. A write to `/ingest` never lands in `KVStateMachine` and vice versa.

2. **No cross-read:** A `GET /kv/{key}` request is served from `KVStateMachine` via `WaitForIndex` + linearizable read. A read against the Bitcask store goes through the consistent-hashing forwarding layer. These two read paths are never mixed.

3. **No shared WAL:** The ingestion WAL (`internal/infrastructure/storage/wal/`) is owned by the Bitcask store. The Raft log persistence (`WALLogStore`, ADR-016) uses a separate WAL instance with its own file path. They share the WAL format code but not state.

### Acknowledged trade-off

Maintaining two paths increases cognitive overhead for new contributors and makes end-to-end testing more complex (two separate integration test paths). This is an **acceptable cost for a learning project** where each path demonstrates different DDIA concepts. In a production system these paths would be unified under a single consensus-backed storage layer.

---

## Consequences

**Positive:**
- Each path remains a clear, isolated demonstration of its respective DDIA concepts.
- Bugs in one path cannot corrupt data in the other path.
- Tests for each path are self-contained.

**Negative:**
- Two storage layers to maintain and understand.
- A `GET` across both namespaces is not directly possible without knowing which path the key was written to.
- v2 will require a non-trivial migration story if the paths are to be unified.

---

## v2 Migration Note

When v2 introduces Joint Consensus (dynamic membership) and Linearizable Read (ReadIndex/Lease Read), the ingestion path will be evaluated for migration to the Raft-backed storage layer. At that point, consistent hashing becomes a routing layer *above* Raft rather than a storage layer alongside it. ADR-018 will be superseded by the v2 storage unification ADR.
