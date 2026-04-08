#!/bin/bash
#
# test_recovery.sh — WAL Crash Recovery E2E 테스트 스크립트
#
# 시나리오:
#   1. 서버 시작
#   2. 이벤트 전송 (curl POST /ingest)
#   3. SIGKILL로 강제 종료 (즉시 종료, graceful shutdown 없음)
#   4. 서버 재시작
#   5. 복구된 이벤트 확인 (로그 또는 stats 엔드포인트)
#
# 사전 조건:
#   - go build ./cmd/main.go로 바이너리 빌드 완료
#   - 포트 8080 사용 가능
#   - ./data 디렉토리 쓰기 가능
#
# 실행:
#   bash scripts/test_recovery.sh

set -euo pipefail

# ============================================================================
# 설정
# ============================================================================

PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BINARY_PATH="${PROJECT_ROOT}/core-x"
WAL_DIR="${PROJECT_ROOT}/data"
WAL_PATH="${WAL_DIR}/events.wal"
LOG_FILE="${PROJECT_ROOT}/recovery_test.log"
SERVER_ADDR="127.0.0.1:8080"
HEALTHZ_URL="http://${SERVER_ADDR}/healthz"
STATS_URL="http://${SERVER_ADDR}/stats"
INGEST_URL="http://${SERVER_ADDR}/ingest"

# 테스트 파라미터
NUM_EVENTS_BEFORE_KILL=5
STARTUP_TIMEOUT_SECONDS=5
SERVER_PORT=8080

# ============================================================================
# 헬퍼 함수
# ============================================================================

log() {
    echo "[$(date +'%H:%M:%S')] $*" | tee -a "${LOG_FILE}"
}

error() {
    echo "[$(date +'%H:%M:%S')] ERROR: $*" | tee -a "${LOG_FILE}" >&2
    exit 1
}

cleanup() {
    log "Cleaning up resources..."
    # 포트에서 실행 중인 프로세스 종료
    if lsof -i ":${SERVER_PORT}" >/dev/null 2>&1; then
        log "Killing process on port ${SERVER_PORT}..."
        lsof -i ":${SERVER_PORT}" -t | xargs kill -9 2>/dev/null || true
        sleep 1
    fi
    log "Cleanup complete"
}

wait_for_healthz() {
    local url="$1"
    local timeout_seconds="${2:-${STARTUP_TIMEOUT_SECONDS}}"
    local elapsed_tenths=0  # 0.1초 단위로 측정
    local interval_tenths=5 # 0.5초 = 5개의 0.1초 유닛
    local timeout_tenths=$((timeout_seconds * 10))

    log "Waiting for server to be ready (timeout: ${timeout_seconds}s)..."
    while [ $elapsed_tenths -lt $timeout_tenths ]; do
        if curl -s "${url}" >/dev/null 2>&1; then
            log "Server is ready"
            return 0
        fi
        sleep 0.5
        elapsed_tenths=$((elapsed_tenths + interval_tenths))
    done

    error "Server failed to start within ${timeout_seconds} seconds"
}

# ============================================================================
# 테스트 실행
# ============================================================================

main() {
    log "=========================================="
    log "WAL Crash Recovery E2E Test"
    log "=========================================="
    log "Project: ${PROJECT_ROOT}"
    log "Binary: ${BINARY_PATH}"
    log "WAL: ${WAL_PATH}"
    log ""

    # Cleanup 설정 (종료 시 자동 실행)
    trap cleanup EXIT

    # ========================================================================
    # Phase 1: 준비 (WAL 디렉토리, 기존 WAL 정리)
    # ========================================================================

    log "Phase 1: Preparing environment..."

    # 이전 테스트 결과 정리
    if [ -f "${LOG_FILE}" ]; then
        rm "${LOG_FILE}"
    fi

    # 이전 WAL 파일 백업
    if [ -f "${WAL_PATH}" ]; then
        local backup_path="${WAL_PATH}.backup.$(date +%s)"
        log "Backing up existing WAL: ${backup_path}"
        mv "${WAL_PATH}" "${backup_path}"
    fi

    # WAL 디렉토리 생성
    mkdir -p "${WAL_DIR}"
    log "WAL directory ready: ${WAL_DIR}"

    # ========================================================================
    # Phase 2: 첫 번째 서버 시작 (초기 상태)
    # ========================================================================

    log ""
    log "Phase 2: Starting server (initial run)..."

    # 서버를 백그라운드에서 실행
    # stderr/stdout를 로그 파일로 리다이렉트
    CORE_X_ADDR="${SERVER_ADDR}" \
    CORE_X_WAL_PATH="${WAL_PATH}" \
    CORE_X_WORKERS=2 \
    CORE_X_BUFFER_DEPTH=20 \
    "${BINARY_PATH}" >> "${LOG_FILE}" 2>&1 &

    local server_pid=$!
    log "Server started with PID ${server_pid}"

    # 서버가 준비될 때까지 대기
    wait_for_healthz "${HEALTHZ_URL}"

    # ========================================================================
    # Phase 3: 이벤트 전송 (WAL에 기록됨)
    # ========================================================================

    log ""
    log "Phase 3: Sending ${NUM_EVENTS_BEFORE_KILL} events..."

    for i in $(seq 1 ${NUM_EVENTS_BEFORE_KILL}); do
        local payload="test-event-${i}"
        local response=$(curl -s -X POST "${INGEST_URL}" \
            -H "Content-Type: application/json" \
            -d "{\"source\":\"test-source\",\"payload\":\"${payload}\"}")

        if [ $? -eq 0 ]; then
            log "  Event ${i} sent: ${payload}"
        else
            error "Failed to send event ${i}"
        fi
    done

    # 이벤트가 WAL에 기록되도록 잠깐 대기
    sleep 0.5

    # ========================================================================
    # Phase 4: SIGKILL로 강제 종료 (graceful shutdown 없음)
    # ========================================================================

    log ""
    log "Phase 4: Killing server with SIGKILL (simulating crash)..."
    kill -9 ${server_pid}
    log "Server killed (PID ${server_pid})"

    # 프로세스가 정말 종료되었는지 확인
    sleep 1
    if kill -0 ${server_pid} 2>/dev/null; then
        error "Process still alive after SIGKILL"
    fi
    log "Process confirmed dead"

    # ========================================================================
    # Phase 5: WAL 파일 검증
    # ========================================================================

    log ""
    log "Phase 5: Verifying WAL file exists..."

    if [ ! -f "${WAL_PATH}" ]; then
        error "WAL file not found: ${WAL_PATH}"
    fi

    local wal_size=$(stat -f%z "${WAL_PATH}" 2>/dev/null || stat -c%s "${WAL_PATH}")
    log "WAL file exists, size: ${wal_size} bytes"

    # ========================================================================
    # Phase 6: 서버 재시작 (crash recovery 실행)
    # ========================================================================

    log ""
    log "Phase 6: Restarting server (recovery will occur)..."

    # 로그 파일을 재시작 지점에서 표시
    local log_before_restart=$(wc -l < "${LOG_FILE}")

    CORE_X_ADDR="${SERVER_ADDR}" \
    CORE_X_WAL_PATH="${WAL_PATH}" \
    CORE_X_WORKERS=2 \
    CORE_X_BUFFER_DEPTH=20 \
    "${BINARY_PATH}" >> "${LOG_FILE}" 2>&1 &

    server_pid=$!
    log "Server restarted with PID ${server_pid}"

    # 서버가 준비될 때까지 대기
    wait_for_healthz "${HEALTHZ_URL}"

    # ========================================================================
    # Phase 7: 복구 검증
    # ========================================================================

    log ""
    log "Phase 7: Verifying crash recovery..."

    # 로그에서 복구 메시지 찾기
    local recovery_log_section=$(tail -n +$((log_before_restart + 1)) "${LOG_FILE}")

    if echo "${recovery_log_section}" | grep -q "crash recovery completed"; then
        # recovered_count를 추출 (macOS 호환: -oP 대신 sed 사용)
        local recovered_count=$(echo "${recovery_log_section}" | grep "crash recovery completed" | sed -E 's/.*recovered_count=([0-9]+).*/\1/' | head -1)
        log "✓ Recovery completed: ${recovered_count} events recovered"

        if [ -z "${recovered_count}" ]; then
            error "Could not extract recovered_count from logs"
        fi

        if [ "${recovered_count}" -eq "${NUM_EVENTS_BEFORE_KILL}" ]; then
            log "✓ Recovered event count matches expected: ${NUM_EVENTS_BEFORE_KILL}"
        else
            error "Recovered event count (${recovered_count}) != Expected (${NUM_EVENTS_BEFORE_KILL})"
        fi
    else
        if echo "${recovery_log_section}" | grep -q "no wal file found"; then
            error "WAL file was not found during recovery"
        else
            error "No recovery message found in logs"
        fi
    fi

    # Stats 엔드포인트 확인 (옵션)
    log ""
    log "Checking stats endpoint..."
    local stats_response=$(curl -s "${STATS_URL}" 2>/dev/null || echo "{}")
    log "Stats: ${stats_response}"

    # ========================================================================
    # Phase 8: 정리 및 최종 보고
    # ========================================================================

    log ""
    log "=========================================="
    log "✓ All tests passed!"
    log "=========================================="
    log "Test log: ${LOG_FILE}"

    # 서버 정상 종료
    kill ${server_pid}
    sleep 1
}

# 메인 함수 실행
main "$@"
