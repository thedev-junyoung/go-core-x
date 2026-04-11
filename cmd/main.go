// cmd/main.go — Core-X Phase 2 제로 할당 HTTP 수집 엔진 + WAL의 Composition Root.
//
// 계층 내 위치: 진입점 (Entry Point). 유일한 "전체 의존성 트리를 아는" 파일이다.
// 이 파일 외에는 어떤 파일도 모든 concrete 인프라 타입을 동시에 import해서는 안 된다.
//
// 책임:
//   - 환경 변수에서 런타임 설정을 읽는다 (12-factor App).
//   - 의존성 그래프 순서에 따라 객체를 생성하고 연결한다 (wiring).
//   - 신호를 수신하고 올바른 순서로 graceful shutdown을 조율한다.
//
// Wiring 순서 (의존성 그래프):
//
//	wal.Writer  ─────────────────────────────────────────┐
//	pool.EventPool  ─────────────────────────────────────┤
//	executor.WorkerPool (Submitter + Stats 구현) ────────┤
//	                                                     ▼
//	                                     application/ingestion.IngestionService
//	                                                     │
//	                                     infrastructure/http.HTTPHandler
//
// 설계 결정 (ADR-001, ADR-002, ADR-004):
//   - 순수 Go stdlib: 프레임워크 없음 = 숨겨진 할당 없음, 매직 미들웨어 체인 없음,
//     핫 경로에 대한 완전한 제어.
//   - Graceful shutdown via 신호 + context: in-flight 잡이 드레인된 후에 프로세스가 종료.
//     DDIA 신뢰성 원칙: silent drop 없음.
//   - WAL shutdown 우선순위: HTTP 수락 중단 → WAL 강제 플러시 → Worker 드레인.
//     "기록되지 않은 데이터는 존재하지 않는다" — WAL이 닫히기 전에 Worker가 종료되어서는 안 된다.
//   - Worker 수 / 버퍼 깊이는 환경 변수로 설정: 올바른 크기 조정은 프로덕션 트래픽 데이터가 필요.
//     하드코딩은 시기상조 최적화이며, 잘못된 값으로 배포하면 성능 병목이 된다.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	appingestion "github.com/junyoung/core-x/internal/application/ingestion"
	"github.com/junyoung/core-x/internal/domain"
	"github.com/junyoung/core-x/internal/infrastructure/cluster"
	"github.com/junyoung/core-x/internal/infrastructure/executor"
	infragrpc "github.com/junyoung/core-x/internal/infrastructure/grpc"
	infrahttp "github.com/junyoung/core-x/internal/infrastructure/http"
	inframetrics "github.com/junyoung/core-x/internal/infrastructure/metrics"
	infraraft "github.com/junyoung/core-x/internal/infrastructure/raft"
	"github.com/junyoung/core-x/internal/infrastructure/pool"
	infrareplication "github.com/junyoung/core-x/internal/infrastructure/replication"
	kvstore "github.com/junyoung/core-x/internal/infrastructure/storage/kv"
	"github.com/junyoung/core-x/internal/infrastructure/storage/wal"
)

func main() {
	// --- 설정 (Configuration) ------------------------------------------------
	// 12-factor 준수: 모든 런타임 설정은 환경 변수로 관리한다.
	// Kubernetes 배포 시 재빌드가 필요 없다 (Phase 3 관심사, 지금 무료로 얻는다).
	addr := envOr("CORE_X_ADDR", ":8080")
	numWorkers := envInt("CORE_X_WORKERS", runtime.NumCPU()*2)
	bufferDepth := envInt("CORE_X_BUFFER_DEPTH", numWorkers*10)
	walPath := envOr("CORE_X_WAL_PATH", "./data/events.wal")
	shutdownTimeout := 30 * time.Second

	// Phase 3: 클러스터 설정 (비어 있으면 단일 노드 모드로 동작).
	nodeID := envOr("CORE_X_NODE_ID", "")
	grpcAddr := envOr("CORE_X_GRPC_ADDR", "")
	peers := envOr("CORE_X_PEERS", "")           // "node-2:localhost:9002,node-3:localhost:9003"
	vnodeCount := envInt("CORE_X_VNODE_COUNT", 150)
	forwardTimeoutStr := envOr("CORE_X_FORWARD_TIMEOUT", "3s")
	forwardTimeout, err := time.ParseDuration(forwardTimeoutStr)
	if err != nil {
		forwardTimeout = 3 * time.Second
	}

	// Phase 3b: Replication 설정.
	role, err := cluster.RoleFromEnv()
	if err != nil {
		slog.Error("invalid CORE_X_ROLE", "err", err)
		os.Exit(1)
	}
	primaryAddr := envOr("CORE_X_PRIMARY_ADDR", "")
	replicaWALPath := envOr("CORE_X_REPLICA_WAL_PATH", "./data/replica.wal")

	// --- 구조화 로깅 (Structured Logging) ------------------------------------
	// slog (Go 1.21 stdlib): 외부 의존성 없음, 구조화 출력,
	// 프로덕션 로그 집계를 위한 JSON 핸들러로 교체 가능.
	// 왜 slog인가: zerolog/zap 대비 의존성이 없고, 성능 차이는 이 유스케이스에서 무의미하다.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	// --- Observability: Prometheus Metrics ------------------------------------
	promReg := inframetrics.NewRegistry()
	ingestMetrics := inframetrics.NewPromIngestMetrics(promReg)

	slog.Info("starting core-x ingestion engine",
		"addr", addr,
		"workers", numWorkers,
		"buffer_depth", bufferDepth,
		"wal_path", walPath,
		"go_version", runtime.Version(),
		"num_cpu", runtime.NumCPU(),
	)

	// --- 인프라: WAL Writer --------------------------------------------------
	// "기록되지 않은 데이터는 존재하지 않는다."
	// WAL은 모든 인프라 컴포넌트 중 가장 먼저 초기화되어야 한다.
	// IngestionService가 Accept하기 전에 기록 경로가 준비되어 있음을 보장한다.
	//
	// 디렉토리 자동 생성:
	// os.MkdirAll은 이미 존재하는 디렉토리에서 no-op이므로 idempotent하다.
	// 0750: owner rwx, group rx, other none (WAL 파일은 보안 민감 데이터를 포함할 수 있다).
	walDir := filepath.Dir(walPath)
	if err := os.MkdirAll(walDir, 0750); err != nil {
		slog.Error("failed to create wal directory", "dir", walDir, "err", err)
		os.Exit(1)
	}

	// SyncInterval 정책 (100ms): 성능과 내구성의 균형점.
	// SyncImmediate는 매 Write마다 fsync로 인해 처리량이 ~10x 감소한다.
	// SyncNever는 OS crash 시 데이터 손실 위험이 있다.
	// 100ms 윈도우 안의 이벤트는 power loss 시 손실 가능 — 수용 가능한 트레이드오프.
	walWriter, err := wal.NewWriter(wal.Config{
		Path:         walPath,
		SyncPolicy:   wal.SyncInterval,
		SyncInterval: 100 * time.Millisecond,
	})
	if err != nil {
		slog.Error("failed to open wal writer", "path", walPath, "err", err)
		os.Exit(1)
	}
	slog.Info("wal writer initialized", "path", walPath, "sync_policy", "interval_100ms")

	// --- 인프라: KV Store (Infrastructure: KV Store) -------------------------
	// Phase 2: Hash Index + WAL = Bitcask model KV store.
	// KVStore implements application/ingestion.WALWriter (WriteEvent).
	// IngestionService.Ingest() → kvStore.WriteEvent() → WAL write + index update.
	kvStore, err := kvstore.NewStore(walWriter, walPath, 1_000_000)
	if err != nil {
		slog.Error("failed to create kv store", "path", walPath, "err", err)
		os.Exit(1)
	}
	slog.Info("kv store initialized", "path", walPath, "max_keys", 1_000_000)

	// --- 인프라: 풀 (Infrastructure: Pool) ----------------------------------
	// sync.Pool 기반 Event 재활용기. 풀 히트 시 0 할당. (ADR-003)
	eventPool := pool.New()

	// --- 인프라: 실행기 (Infrastructure: Executor) --------------------------
	// Phase 2 프로세서: WAL 기록은 Ingest() 경로에서 이미 완료되었으므로
	// Worker는 다운스트림 처리(집계, 포워딩 등)에만 집중한다.
	// domain.EventProcessorFunc 어댑터 덕분에 named struct 없이 func 리터럴이 주입 가능하다.
	processor := domain.EventProcessorFunc(func(e *domain.Event) {
		slog.Debug("event processed",
			"source", e.Source,
			"received_at", e.ReceivedAt,
			"payload_len", len(e.Payload),
		)
		// Phase 3 훅: 다운스트림 전송, 집계, 변환 등이 여기에 추가된다.
	})

	// workerPool은 Submitter와 Stats를 모두 구현한다.
	// 두 인터페이스를 하나의 concrete 타입이 구현하게 한 이유:
	// 쓰기(Submit)와 읽기(Stats) 경로가 동일한 채널과 카운터를 공유하기 때문이다.
	workerPool := executor.NewWorkerPool(executor.Config{
		NumWorkers:    numWorkers,
		JobBufferSize: bufferDepth,
		Processor:     processor,
		EventPool:     eventPool,
	})

	// --- Crash Recovery (크래시 복구) ------------------------------------------
	// Phase 2의 핵심: KV Store의 Hash Index를 WAL 재생으로 재구축.
	// 동시에 recovered 이벤트를 workerPool에 재전송하여 처리 복구도 진행.
	//
	// 절차:
	//   1. kvStore.Recover(callback) → WAL 전체 스캔
	//   2. 각 레코드: index 재구축 + callback 호출
	//   3. callback: workerPool.Submit(e) → 이벤트 처리 재진행
	//   4. ErrTruncated: 정상 처리 (마지막 불완전 레코드는 버림)
	//   5. ErrCorrupted/ErrChecksumMismatch: fatal 에러
	//
	// 오프라인 복구 보증:
	//   - kvStore.Recover는 index rebuild만 담당 (WAL에 다시 기록하지 않음)
	//   - callback으로 workerPool.Submit을 전달하면 처리 복구도 함께 진행
	//   - 콜백 패턴으로 계층 위반 없음 (kv ← application/executor)
	recoveredCount, err := kvStore.Recover(func(e *domain.Event) {
		// Recovered event: already in WAL, just submit to worker for processing
		if !workerPool.Submit(e) {
			// Theoretically should not happen during recovery (single-threaded)
			slog.Warn("recovered event submission failed", "source", e.Source)
		}
	})
	if err != nil {
		slog.Error("crash recovery failed", "wal_path", walPath, "err", err)
		os.Exit(1)
	}
	if recoveredCount > 0 {
		slog.Info("crash recovery completed",
			"recovered_count", recoveredCount,
			"index_size", kvStore.Len(),
		)
	}

	// --- 애플리케이션 계층 (Application Layer) --------------------------------
	// IngestionService는 Ingest 유스케이스를 소유한다.
	// concrete 인프라 타입이 아닌 포트(인터페이스)에 의존하므로
	// 실제 풀이나 executor 없이도 테스트 가능하다.
	//
	// Phase 2 변경: kvStore가 WALWriter 포트를 구현한다.
	// IngestionService.Ingest() → kvStore.WriteEvent() → WAL 기록 + index 업데이트.
	// KVStore는 내부적으로 walWriter를 사용하여 투명하게 WAL 기록 + index를 처리한다.
	svc := appingestion.NewIngestionService(workerPool, eventPool, kvStore)
	svc.SetMetrics(ingestMetrics)

	// Periodically update queue depth gauge for Prometheus.
	// QueueDepth is a gauge (not a counter), so it must be polled.
	// 5s interval: low overhead, sufficient resolution for alerting.
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			ingestMetrics.RecordQueueDepth(workerPool.QueueDepth())
		}
	}()

	// --- Phase 3: 클러스터 초기화 -------------------------------------------
	// CORE_X_NODE_ID가 설정되어 있으면 클러스터 모드로 동작한다.
	// 설정이 없으면 단일 노드 모드 (Phase 1/2 동작 그대로).
	var (
		ring      *cluster.Ring
		forwarder *infragrpc.Forwarder
		grpcSrv   *infragrpc.Server
	)

	if nodeID != "" && grpcAddr != "" {
		ring = cluster.NewRing(vnodeCount)

		// 자신을 ring에 등록.
		selfNode := cluster.NewNode(nodeID, grpcAddr)
		ring.AddNode(selfNode)

		// 피어 노드 파싱 및 ring 등록.
		// 형식: "node-2:localhost:9002,node-3:localhost:9003"
		if peers != "" {
			for _, peer := range splitPeers(peers) {
				if peer.id != "" && peer.addr != "" {
					ring.AddNode(cluster.NewNode(peer.id, peer.addr))
				}
			}
		}

		// health probe 시작.
		membership := cluster.NewMembership(ring)
		membershipCtx, membershipCancel := context.WithCancel(context.Background())
		defer membershipCancel()
		membership.Start(membershipCtx)

		// gRPC 포워더.
		clientPool := infragrpc.NewClientPool()
		forwarder = infragrpc.NewForwarder(clientPool)
		defer clientPool.Close()

		// gRPC 서버 시작.
		var grpcErr error
		grpcSrv, grpcErr = infragrpc.NewServer(grpcAddr, svc)
		if grpcErr != nil {
			slog.Error("failed to start grpc server", "addr", grpcAddr, "err", grpcErr)
			os.Exit(1)
		}

		slog.Info("cluster mode enabled",
			"node_id", nodeID,
			"grpc_addr", grpcAddr,
			"ring_nodes", ring.Len(),
			"vnode_count", vnodeCount,
		)
	}

	// --- Phase 3b: Replication 초기화 ------------------------------------------
	var replLag *infrareplication.ReplicationLag

	if role == cluster.RolePrimary {
		if grpcSrv == nil {
			slog.Error("replication: primary role requires CORE_X_NODE_ID and CORE_X_GRPC_ADDR to be set")
			os.Exit(1)
		}

		replLag = infrareplication.NewReplicationLag()

		walReadFile, err := os.Open(walPath)
		if err != nil {
			slog.Error("replication: failed to open wal for streaming", "path", walPath, "err", err)
			os.Exit(1)
		}
		defer walReadFile.Close()

		streamer := infrareplication.NewStreamer(walReadFile, walWriter.CompactionNotify, replLag, 10*time.Millisecond)
		replServer := infrareplication.NewReplicationServer(streamer)
		grpcSrv.RegisterReplicationService(replServer)

		slog.Info("replication: primary mode enabled", "wal_path", walPath)

	} else if role == cluster.RoleReplica {
		replLag = infrareplication.NewReplicationLag()

		replicaDir := filepath.Dir(replicaWALPath)
		if err := os.MkdirAll(replicaDir, 0750); err != nil {
			slog.Error("replication: failed to create replica wal dir", "err", err)
			os.Exit(1)
		}

		receiver, err := infrareplication.NewReceiver(replicaWALPath)
		if err != nil {
			slog.Error("replication: failed to open replica wal", "path", replicaWALPath, "err", err)
			os.Exit(1)
		}

		replClient := infrareplication.NewReplicationClient(
			primaryAddr,
			nodeID,
			receiver,
			replLag,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)

		replCtx, replCancel := context.WithCancel(context.Background())
		defer replCancel()

		go func() {
			if err := replClient.Run(replCtx); err != nil && replCtx.Err() == nil {
				slog.Error("replication client stopped unexpectedly", "err", err)
			}
		}()

		slog.Info("replication: replica mode enabled",
			"primary_addr", primaryAddr,
			"replica_wal_path", replicaWALPath,
		)
	}

	// --- Phase 5a: Raft Leader Election ------------------------------------
	// Raft is only activated in cluster mode (nodeID + grpcAddr + grpcSrv configured).
	// Standalone mode skips Raft entirely.
	var raftNode *infraraft.RaftNode

	if nodeID != "" && grpcAddr != "" && grpcSrv != nil {
		var peerAddrs []string
		if ring != nil {
			for _, n := range ring.Nodes() {
				if n.ID != nodeID {
					peerAddrs = append(peerAddrs, n.Addr)
				}
			}
		}

		var peerClients *infraraft.PeerClients
		if len(peerAddrs) > 0 {
			var pcErr error
			peerClients, pcErr = infraraft.NewPeerClients(peerAddrs)
			if pcErr != nil {
				slog.Error("raft: failed to dial peers", "err", pcErr)
				os.Exit(1)
			}
			defer peerClients.Close()
		}

		raftNode = infraraft.NewRaftNode(nodeID, peerClients, nil) // TODO(phase5b): inject FileMetaStore
		raftServer := infraraft.NewRaftServer(raftNode)
		grpcSrv.RegisterRaftService(raftServer)

		raftCtx, raftCancel := context.WithCancel(context.Background())
		defer raftCancel()

		go raftNode.Run(raftCtx)

		slog.Info("raft: started", "node_id", nodeID, "peers", peerAddrs)
	}

	inframetrics.RegisterReplicationMetrics(promReg, replLag)
	inframetrics.RegisterClusterMetrics(promReg, ring)
	inframetrics.RegisterRaftMetrics(promReg, raftNode)

	// gRPC 서버 goroutine은 replication 서비스 등록 이후에 시작한다.
	// RegisterReplicationService → Serve 순서를 보장해 race를 제거한다.
	if grpcSrv != nil {
		grpcSrv.RegisterKVService(infragrpc.NewGRPCKVServer(kvStore))
		go func() {
			slog.Info("grpc server listening", "addr", grpcSrv.Addr)
			if err := grpcSrv.Serve(); err != nil {
				slog.Error("grpc server error", "err", err)
			}
		}()
	}

	// --- HTTP 라우터 (HTTP Router) -------------------------------------------
	// 순수 stdlib ServeMux. 미들웨어 프레임워크 없음.
	// 크로스커팅 관심사(인증, 추적)는 필요 시 http.Handler 래퍼로 구현된다 —
	// 프레임워크 의존성을 추가하지 않는다.
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

	// --- HTTP 서버 (HTTP Server) ---------------------------------------------
	// 명시적 타임아웃은 선택 사항이 아니다.
	// 타임아웃 없이는 느린 클라이언트가 연결을 무한 점유해 파일 디스크립터를 고갈시킨다.
	// ReadHeaderTimeout: Slowloris 류의 공격(헤더를 극도로 천천히 전송)을 방지한다.
	// WriteTimeout: 느린 클라이언트가 응답 소비를 지연해 worker를 점유하는 것을 방지한다.
	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,  // Slowloris 방어
		ReadTimeout:       10 * time.Second, // 요청 바디 수신 상한
		WriteTimeout:      10 * time.Second, // 응답 전송 상한
		IdleTimeout:       60 * time.Second, // keep-alive 연결 유휴 상한
	}

	// --- Graceful Shutdown ---------------------------------------------------
	// 별도 고루틴에서 신호를 처리한다; main 고루틴은 ListenAndServe에서 블로킹한다.
	//
	// Shutdown 순서가 중요하다 (DDIA 신뢰성):
	//   1. server.Shutdown → 새 HTTP 연결 수락 중단 (새 Submit() 진입 차단)
	//   2. walWriter.Close() → 메모리 버퍼를 fsync로 강제 플러시 (데이터 보존 보장)
	//   3. workerPool.Shutdown → 채널 드레인, 그 다음 worker 종료
	//
	// WAL이 Step 2에 위치하는 이유:
	//   - Step 1 이후: 새 이벤트가 들어오지 않으므로 WAL에 기록 중인 이벤트가 없다.
	//   - Step 3 이전: Worker가 처리 중인 이벤트는 이미 WAL에 기록 완료된 상태다.
	//     (Ingest → WAL 기록 → Submit 순서 보장, service.go 참조)
	//   → WAL을 닫은 후 Worker를 드레인해도 데이터 손실이 없다.
	//
	// 순서를 뒤집으면 경쟁 조건이 발생한다:
	//   workerPool.Shutdown이 채널을 먼저 닫으면,
	//   in-flight Submit()이 닫힌 채널로 전송을 시도해 패닉이 발생한다.
	quit := make(chan os.Signal, 1) // 버퍼 크기 1: 신호 전송 시 고루틴이 블로킹되지 않도록
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-quit
		slog.Info("shutdown signal received", "signal", sig)

		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()

		// Step 1: 새 HTTP 요청 수락 중단.
		// 이미 수락된 요청은 WriteTimeout까지 처리를 계속한다.
		if err := server.Shutdown(ctx); err != nil {
			slog.Error("http server shutdown error", "err", err)
		}

		// Step 2: WAL 버퍼를 강제 플러시하고 파일을 닫는다.
		// server.Shutdown 이후이므로 새 WriteEvent() 호출은 없다.
		// "기록되지 않은 데이터는 존재하지 않는다" — fsync 완료 전까지 종료하지 않는다.
		if err := walWriter.Close(); err != nil {
			slog.Error("wal writer close error", "err", err)
		} else {
			slog.Info("wal writer flushed and closed")
		}

		// Step 2.5: KV Store read handle을 닫는다.
		// kvStore는 read-only 파일 핸들을 보유하므로 명시적으로 닫아야 함.
		// walWriter.Close() 이후이므로 새 WriteEvent() 호출은 없고,
		// 모든 새 Get() 호출도 없다 (HTTP 서버가 이미 shutdown).
		if err := kvStore.Close(); err != nil {
			slog.Error("kv store close error", "err", err)
		} else {
			slog.Info("kv store closed")
		}

		// Step 3: gRPC 서버 graceful stop (클러스터 모드).
		if grpcSrv != nil {
			grpcSrv.Stop()
			slog.Info("grpc server stopped")
		}

		// Step 4: jobCh를 닫고 모든 worker가 드레인·종료할 때까지 대기.
		// server.Shutdown 이후이므로 Submit()이 닫힌 채널로 전송하는 경쟁이 없다.
		// WAL과 kvStore가 이미 닫혔으므로 Worker 처리 결과는 이미 영속화된 상태다.
		workerPool.Shutdown(ctx)
	}()

	// --- 서버 시작 -----------------------------------------------------------
	slog.Info("http server listening", "addr", addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("http server error", "err", err)
		os.Exit(1)
	}

	slog.Info("core-x ingestion engine stopped cleanly")
}

// envOr는 지정된 환경 변수 값을 반환한다.
// 변수가 설정되지 않았거나 비어 있으면 fallback을 반환한다.
//
// 왜 os.LookupEnv가 아닌 os.Getenv인가?
// 빈 문자열과 미설정을 구분할 필요가 없다. 빈 값은 미설정과 동일하게 처리한다.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

type peerEntry struct {
	id   string
	addr string
}

// splitPeers parses the CORE_X_PEERS environment variable.
// Expected format: "node-2:localhost:9002,node-3:localhost:9003"
// Each entry is "nodeID:host:port".
func splitPeers(s string) []peerEntry {
	var result []peerEntry
	for _, entry := range strings.Split(s, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		// Split on first colon only to get nodeID; the rest is the address.
		idx := strings.Index(entry, ":")
		if idx < 0 {
			continue
		}
		result = append(result, peerEntry{
			id:   entry[:idx],
			addr: entry[idx+1:],
		})
	}
	return result
}

// envInt는 지정된 환경 변수의 정수 값을 반환한다.
// 미설정이거나 파싱 불가능하면 fallback을 반환한다.
//
// 잘못된 값(예: "abc")에 대해 fallback을 조용히 사용하지 않고 경고를 로깅한다.
// 이유: 잘못된 설정은 운영자가 알아야 할 문제이며,
// 조용한 폴백은 예상치 못한 동작으로 이어질 수 있다.
func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
		slog.Warn("invalid integer env var, using fallback", "key", key, "fallback", fallback)
	}
	return fallback
}

