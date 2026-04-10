# Phase 3 gRPC 요청 흐름 시퀀스 다이어그램

Phase 3에서 추가된 클러스터 모드의 세 가지 핵심 흐름을 Mermaid sequenceDiagram으로 기술한다.
각 다이어그램은 실제 구현 코드(`handler.go`, `forward.go`, `client.go`, `ring.go`, `membership.go`)를 기반으로 작성되었다.

---

## 1. Forward Path — 담당 노드가 다른 피어인 경우

클라이언트가 `POST /ingest`를 보냈을 때, consistent hashing 결과 담당 노드가 자신이 아닌 경우의 흐름.
`Ring.Lookup(source)`으로 담당 노드를 결정하고, 노드가 healthy하면 `Forwarder.Forward()`가
`ClientPool`을 통해 피어에게 gRPC `Ingest` RPC를 호출한다.
피어의 `GRPCIngestionServer`는 동일한 `IngestionService`를 재사용하여 로컬 처리를 완료한다.

```mermaid
sequenceDiagram
    autonumber
    participant Client as HTTP Client
    participant Handler as HTTPHandler<br/>(node-1)
    participant Ring as cluster.Ring
    participant Forwarder as grpc.Forwarder
    participant Pool as grpc.ClientPool
    participant PeerGRPC as GRPCIngestionServer<br/>(node-2)
    participant Svc as IngestionService<br/>(node-2)

    Client->>Handler: POST /ingest {source, payload}
    Handler->>Handler: JSON decode + field validation

    Note over Handler,Ring: ring != nil → 클러스터 모드
    Handler->>Ring: Lookup(source)
    Ring-->>Handler: target=node-2, ok=true

    Note over Handler: target.ID != selfID → forward 필요
    Handler->>Handler: target.IsHealthy() → true
    Handler->>Handler: context.WithTimeout(r.Context(), forwardTimeout=3s)

    Handler->>Forwarder: Forward(ctx, node-2, source, payload)
    Forwarder->>Pool: Get(node-2)

    alt 기존 conn 있음
        Pool-->>Forwarder: cached ClientConn → IngestionServiceClient
    else 첫 번째 요청 (lazy init)
        Pool->>Pool: grpc.NewClient(node-2.Addr)
        Pool-->>Forwarder: new IngestionServiceClient
    end

    Forwarder->>PeerGRPC: pb.Ingest(IngestRequest{source, payload})
    PeerGRPC->>Svc: svc.Ingest(source, payload)
    Svc-->>PeerGRPC: nil (success)
    PeerGRPC-->>Forwarder: IngestResponse{Ok: true}

    Forwarder-->>Handler: nil
    Handler-->>Client: 202 Accepted
```

---

## 2. Local Path — 자신이 담당 노드인 경우 (단일 노드 모드 포함)

`Ring.Lookup` 결과 자신이 담당 노드이거나(`target.ID == selfID`), ring이 nil인 단일 노드 모드의 흐름.
두 경우 모두 `IngestionService.Ingest()`를 직접 호출하는 동일한 로컬 처리 경로로 진입한다.
KV Store가 WAL에 기록하고 Worker Pool에 이벤트를 제출하는 구조는 Phase 2와 동일하다.

```mermaid
sequenceDiagram
    autonumber
    participant Client as HTTP Client
    participant Handler as HTTPHandler
    participant Ring as cluster.Ring
    participant Svc as IngestionService
    participant KV as kvstore.Store<br/>(WALWriter port)
    participant WAL as wal.Writer
    participant WP as executor.WorkerPool

    Client->>Handler: POST /ingest {source, payload}
    Handler->>Handler: JSON decode + field validation

    alt 클러스터 모드 (ring != nil)
        Handler->>Ring: Lookup(source)
        Ring-->>Handler: target=node-1 (self), ok=true
        Note over Handler: target.ID == selfID → 로컬 처리
    else 단일 노드 모드 (ring == nil)
        Note over Handler: ring == nil → 로컬 처리로 직행
    end

    Handler->>Svc: svc.Ingest(source, payload)
    Svc->>Svc: pool.Get() → *domain.Event (zero-alloc on pool hit)
    Svc->>KV: kvStore.WriteEvent(event)
    KV->>WAL: WAL append (length-prefixed + CRC32)
    WAL-->>KV: offset
    KV->>KV: index[source] = offset
    KV-->>Svc: nil

    Svc->>WP: workerPool.Submit(event)

    alt worker pool에 여유 있음
        WP-->>Svc: true
        Svc-->>Handler: nil
        Handler-->>Client: 202 Accepted
    else 버퍼 포화 (ErrOverloaded)
        WP-->>Svc: false
        Svc-->>Handler: ErrOverloaded
        Handler-->>Client: 429 Too Many Requests
    end
```

---

## 3. Health Probe — 백그라운드 노드 상태 감지 루프

`cluster.Membership`이 백그라운드 goroutine에서 5초 간격으로 모든 피어 노드의 gRPC 연결 상태를 확인한다.
각 probe는 독립적인 `grpc.ClientConn`을 생성하고 `connectivity.Ready` 상태 전이를 2초 타임아웃 내에 기다린다.
상태 변화(healthy ↔ unhealthy)가 감지되면 `node.setHealthy()`가 `atomic.Bool`을 갱신하여
HTTP forwarding 경로의 `target.IsHealthy()` 판단에 즉시 반영된다.

```mermaid
sequenceDiagram
    autonumber
    participant Main as cmd/main.go
    participant Membership as cluster.Membership
    participant Ring as cluster.Ring
    participant Node as cluster.Node<br/>(atomic.Bool healthy)
    participant GRPCConn as grpc.ClientConn<br/>(probe-only, ephemeral)

    Main->>Membership: membership.Start(ctx)
    Note over Membership: 백그라운드 goroutine 시작

    loop 5초마다 (defaultProbeInterval)
        Membership->>Membership: ticker.C fires
        Membership->>Ring: ring.Nodes()
        Ring-->>Membership: []*Node snapshot

        loop 각 peer node에 대해
            Membership->>GRPCConn: grpc.NewClient(node.Addr)
            Membership->>GRPCConn: conn.Connect()

            loop connectivity.Ready 또는 ctx 만료까지
                GRPCConn-->>Membership: state (Connecting / Ready / ...)
                alt state == connectivity.Ready
                    Note over Membership: probe 성공
                else WaitForStateChange 반환 (ctx 만료)
                    Note over Membership: probe 실패 (2s timeout)
                end
            end

            Membership->>GRPCConn: conn.Close() (defer)

            Membership->>Node: wasHealthy = node.IsHealthy()
            Membership->>Node: node.setHealthy(healthy)

            alt 상태 변화 감지
                alt unhealthy → healthy
                    Membership->>Membership: slog.Info("node recovered")
                else healthy → unhealthy
                    Membership->>Membership: slog.Warn("node marked unhealthy")
                end
            end
        end
    end

    Note over Membership: ctx.Done() 수신 시 goroutine 종료
    Main->>Membership: membershipCancel() (defer)
```
