# ADR-008: Basic Replication — Async WAL Streaming

## Status
Accepted

## Context

Phase 3a completed consistent-hash partitioning with gRPC forwarding. The next reliability requirement is **replication**: if the only node owning a partition fails, its data is gone. The simplest durable replication primitive is WAL streaming — the Primary tails its own WAL file and pushes raw bytes to Replica(s) over a long-lived gRPC stream.

### Constraints considered

- **No index on Replica (Phase 3b scope)**: Replica stores raw WAL bytes but does not rebuild a KV index. Read serving from Replica is deferred to Phase 5 (index rebuild). Phase 3b only guarantees *data durability*, not *read scale-out*.
- **Async, not synchronous**: Primary does not wait for Replica acknowledgment before returning 202 to the client. This is an intentional availability-over-consistency trade-off (see Consequences).
- **Compaction awareness**: The Primary's WAL undergoes stop-the-world compaction (ADR-006). The Streamer must detect compaction (via a `CompactionNotify` channel) and reset its read offset to 0 when a new compacted WAL replaces the old one.

## Decision

### Architecture

```
Primary (cmd/main.go, role=primary)
  ├── WAL Writer (append-only file)
  ├── Streamer (tails WAL, pushes RawEntry via gRPC stream)
  │     └── polls WAL on EOF (10ms interval)
  │     └── resets offset on CompactionNotify signal
  └── ReplicationServer (wraps Streamer, implements pb.ReplicationServiceServer)

Replica (cmd/main.go, role=replica)
  ├── ReplicationClient (connects to primary, opens StreamWAL RPC)
  │     └── reconnects with exponential backoff on disconnect
  └── Receiver (appends raw WAL bytes + fsync per entry)
        └── replica.wal (separate file from primary's WAL)
```

### Proto: StreamWAL RPC

```protobuf
service ReplicationService {
  rpc StreamWAL(StreamWALRequest) returns (stream WALEntry);
}
message StreamWALRequest { string replica_id = 1; int64 start_offset = 2; }
message WALEntry          { int64 offset = 1; bytes raw_data = 2; int64 record_size = 3; }
```

The Replica sends its `start_offset` (= `PersistedOffset()` of its local file) on connect. The Primary starts streaming from that offset, skipping already-replicated data after a reconnect.

### Durability guarantee

- `Receiver.Append()` calls `file.Sync()` (fsync) after every entry. A Replica crash loses at most the in-flight entry being written.
- Replica WAL path is separate (`CORE_X_REPLICA_WAL_PATH`) to avoid interleaving with the Primary's ingestion WAL.

### Lag tracking

`ReplicationLag` tracks two atomic counters:
- `Bytes()`: size of entries streamed but not yet acknowledged (set by Streamer, cleared on Replica ACK — currently approximated as bytes sent)
- `ReconnectCount()`: number of times the Replica reconnected

Both are exposed on `GET /stats` (JSON) and, starting Phase 4, via Prometheus `core_x_replication_lag_bytes`.

### Role assignment

Static via `CORE_X_ROLE` environment variable (`primary` | `replica` | empty for standalone). Automatic failover (Raft-based role promotion) is Phase 5.

## Why Async Over Synchronous Replication

| Property | Async (chosen) | Sync |
|----------|----------------|------|
| Write latency | Unchanged (Primary doesn't wait) | +RTT to Replica |
| Availability | Primary accepts writes even if Replica is down | Primary blocks if Replica is slow/down |
| Durability on Primary crash | Last ~10ms window may be lost on Replica | Zero loss |
| Complexity | Streamer + Receiver; no ACK coordination | Requires quorum ACK before 202 response |

Core-X is an **ingestion engine** — write throughput and availability are primary goals. Losing the tail of a WAL on a rare Primary crash is a more acceptable trade-off than adding RTT to every write.

## Consequences

**Positive:**
- Zero write-path latency impact — Primary does not block on Replica I/O
- Simple failure mode: if Replica is unreachable, Primary continues; Replica reconnects and resumes from its persisted offset
- Compaction-safe: Streamer resets offset on compaction signal, no stale-offset reads

**Negative:**
- **Replica read lag**: Replica WAL may be behind Primary by up to one poll interval (10ms) + network RTT
- **No automatic failover**: If Primary crashes, Replica has the data but cannot serve writes or reads automatically. Operator must manually promote Replica (set `CORE_X_ROLE=primary`)
- **Replica has no KV index**: Raw WAL bytes only. Read path on Replica is not available until Phase 5 adds index rebuild

**Known limitation:**
Phase 3b intentionally defers two concerns to later phases:
- Phase 4: Read path (`GET /kv/{key}`) — requires Primary's index; Replica reads redirect to Primary via ring lookup
- Phase 5: Raft-based failover and Replica index rebuild for read scale-out
