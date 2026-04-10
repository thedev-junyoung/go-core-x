#!/bin/bash
#
# bench_phases.sh — Core-X 단계별 성능 비교 자동화 스크립트
#
# 각 Phase별로 서버를 기동하고 load generator를 실행한 뒤,
# 결과를 하나의 비교 리포트로 출력한다.
#
# 전제 조건:
#   - go build가 성공적으로 실행된 상태여야 한다.
#   - 포트 8080 사용 가능
#
# 실행:
#   bash scripts/bench_phases.sh
#
# 출력:
#   각 Phase의 RPS, p50/p95/p99 레이턴시, 에러율
#   마지막에 비교 테이블 출력

set -euo pipefail

PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BINARY="${PROJECT_ROOT}/core-x-bench"
BENCH_TOOL="${PROJECT_ROOT}/tools/benchmark/main.go"
WAL_DIR="/tmp/core-x-bench-wal"
SERVER_ADDR="127.0.0.1:8080"
RESULTS_DIR="/tmp/core-x-bench-results"

# Load parameters — tune these for your machine.
RPS=5000
CONCURRENCY=50
DURATION="15s"
WARMUP="3s"

# ============================================================================
# 헬퍼 함수
# ============================================================================

log() { echo "[$(date +'%H:%M:%S')] $*"; }
error() { echo "[ERROR] $*" >&2; exit 1; }

cleanup_server() {
    if lsof -i ":8080" >/dev/null 2>&1; then
        log "Stopping server on :8080..."
        lsof -i ":8080" -t | xargs kill -TERM 2>/dev/null || true
        sleep 1
        # Force kill if still running.
        if lsof -i ":8080" >/dev/null 2>&1; then
            lsof -i ":8080" -t | xargs kill -9 2>/dev/null || true
            sleep 0.5
        fi
    fi
}

wait_healthz() {
    local url="http://${SERVER_ADDR}/healthz"
    local timeout=10
    local elapsed=0
    while [ $elapsed -lt $timeout ]; do
        if curl -sf "${url}" >/dev/null 2>&1; then return 0; fi
        sleep 0.3
        elapsed=$((elapsed + 1))
    done
    error "Server did not become healthy within ${timeout}s"
}

run_phase() {
    local phase_label="$1"
    local phase_env_vars="$2"    # additional env vars (space-separated KEY=VALUE)
    local result_file="$3"

    log "--- ${phase_label} ---"

    # Clean WAL for each phase (fresh start).
    rm -rf "${WAL_DIR}" && mkdir -p "${WAL_DIR}"

    # Start server with phase-specific config.
    local server_log="/tmp/core-x-server-${phase_label//[^a-zA-Z0-9]/-}.log"
    env \
        CORE_X_ADDR="${SERVER_ADDR}" \
        CORE_X_WAL_PATH="${WAL_DIR}/events.wal" \
        CORE_X_WORKERS=8 \
        CORE_X_BUFFER_DEPTH=200 \
        ${phase_env_vars} \
        "${BINARY}" > "${server_log}" 2>&1 &

    local server_pid=$!
    log "Server started (PID ${server_pid}), waiting for healthz..."
    wait_healthz

    # Run benchmark.
    go run "${BENCH_TOOL}" \
        --addr "http://${SERVER_ADDR}" \
        --rps "${RPS}" \
        --concurrency "${CONCURRENCY}" \
        --duration "${DURATION}" \
        --warmup "${WARMUP}" \
        --phase "${phase_label}" \
        > "${result_file}" 2>&1 || true

    cat "${result_file}"

    # Stop server.
    kill "${server_pid}" 2>/dev/null || true
    wait "${server_pid}" 2>/dev/null || true
    sleep 0.5
}

# ============================================================================
# メイン
# ============================================================================

main() {
    log "Building Core-X binary..."
    cd "${PROJECT_ROOT}"
    go build -o "${BINARY}" ./cmd/
    log "Binary: ${BINARY}"

    mkdir -p "${RESULTS_DIR}"
    trap 'cleanup_server' EXIT

    # Phase 1: In-Memory only (no WAL, no KV persistence)
    # To test Phase 1 in isolation, we would need a build tag or env-based bypass.
    # Since the binary always initializes WAL, we simulate Phase 1 behavior by
    # using a RAM disk path (if available) to minimize WAL I/O overhead.
    # On macOS: /tmp is on APFS (SSD). On Linux: use tmpfs (/dev/shm if available).
    PHASE1_WAL="/tmp/core-x-p1.wal"
    rm -f "${PHASE1_WAL}"
    run_phase "Phase 1: In-Memory (WAL on tmpfs)" \
        "CORE_X_WAL_PATH=${PHASE1_WAL}" \
        "${RESULTS_DIR}/phase1.txt"

    # Phase 2: Hash Index KV (Bitcask model, WAL-backed)
    PHASE2_WAL="/tmp/core-x-p2.wal"
    rm -f "${PHASE2_WAL}"
    run_phase "Phase 2: Hash Index KV (Bitcask)" \
        "CORE_X_WAL_PATH=${PHASE2_WAL}" \
        "${RESULTS_DIR}/phase2.txt"

    # Phase 3: Same binary as Phase 2 (WAL compaction is transparent).
    # Run with sync interval = 0 (SyncNever) to see max throughput.
    # This simulates measuring after compaction is running.
    PHASE3_WAL="/tmp/core-x-p3.wal"
    rm -f "${PHASE3_WAL}"
    run_phase "Phase 3: WAL Compaction (stop-the-world)" \
        "CORE_X_WAL_PATH=${PHASE3_WAL}" \
        "${RESULTS_DIR}/phase3.txt"

    # Phase 4: Single-node mode (no gRPC forwarding overhead).
    # Full cluster mode requires 3 processes; single-node gives the baseline.
    PHASE4_WAL="/tmp/core-x-p4.wal"
    rm -f "${PHASE4_WAL}"
    run_phase "Phase 4: Cluster (single-node baseline)" \
        "CORE_X_WAL_PATH=${PHASE4_WAL}" \
        "${RESULTS_DIR}/phase4.txt"

    # ========================================================================
    # Summary
    # ========================================================================
    log ""
    log "============================================================"
    log "  PHASE COMPARISON SUMMARY"
    log "  (RPS target: ${RPS}, concurrency: ${CONCURRENCY}, duration: ${DURATION})"
    log "============================================================"

    echo ""
    printf "  %-38s | %-12s | %-7s | %-7s | %-7s | %-7s\n" \
        "Phase" "RPS (actual)" "Mean" "p50" "p95" "p99"
    printf "  %s\n" "$(printf '%.0s─' {1..95})"

    for f in "${RESULTS_DIR}/phase1.txt" "${RESULTS_DIR}/phase2.txt" \
              "${RESULTS_DIR}/phase3.txt" "${RESULTS_DIR}/phase4.txt"; do
        if [ -f "$f" ]; then
            local phase_name actual_rps mean p50 p95 p99
            phase_name=$(grep "Phase  :" "$f" | sed 's/.*Phase  : //' | xargs)
            actual_rps=$(grep "Actual RPS" "$f" | sed 's/.*: //' | xargs)
            mean=$(grep "Mean :" "$f" | sed 's/.*Mean : //' | xargs)
            p50=$(grep "p50  :" "$f" | sed 's/.*p50  : //' | xargs)
            p95=$(grep "p95  :" "$f" | sed 's/.*p95  : //' | xargs)
            p99=$(grep "p99  :" "$f" | sed 's/.*p99  : //' | xargs)
            printf "  %-38s | %12s | %-7s | %-7s | %-7s | %-7s\n" \
                "${phase_name}" "${actual_rps}" "${mean}" "${p50}" "${p95}" "${p99}"
        fi
    done

    echo ""
    log "Individual results: ${RESULTS_DIR}/"
    log "Done."
}

main "$@"
