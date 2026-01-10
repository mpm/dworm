#!/bin/bash
# Common utilities for E2E tests

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
DWORM="$PROJECT_ROOT/bin/dworm"
DEVCONTAINER_PATH="$PROJECT_ROOT"
TEST_TIMEOUT=60

# Logging functions
log_info() { echo "[INFO] $*"; }
log_error() { echo "[ERROR] $*" >&2; }
log_skip() { echo "[SKIP] $*"; }
log_pass() { echo "[PASS] $*"; }
log_fail() { echo "[FAIL] $*"; }

# Get container ID for the devcontainer
get_container_id() {
    docker ps -q --filter "label=devcontainer.local_folder=$DEVCONTAINER_PATH" | head -1
}

# Wait for container to be ready
wait_for_container() {
    local max_wait="${1:-30}"
    local count=0
    while [[ $count -lt $max_wait ]]; do
        local container_id
        container_id=$(get_container_id)
        if [[ -n "$container_id" ]]; then
            return 0
        fi
        sleep 1
        ((count++))
    done
    return 1
}

# Start dworm in background
start_dworm() {
    local extra_args="${1:-}"
    log_info "Starting dworm..."
    # shellcheck disable=SC2086
    "$DWORM" up "$DEVCONTAINER_PATH" --daemon $extra_args &
    DWORM_PID=$!

    # Wait for container to be ready
    if ! wait_for_container 30; then
        log_error "Timeout waiting for container to start"
        return 1
    fi

    # Give endpoint time to initialize
    sleep 2
    log_info "dworm started (PID: $DWORM_PID)"
}

# Stop dworm and container
stop_dworm() {
    log_info "Stopping dworm..."
    if [[ -n "${DWORM_PID:-}" ]]; then
        kill "$DWORM_PID" 2>/dev/null || true
        wait "$DWORM_PID" 2>/dev/null || true
    fi
    "$DWORM" down "$DEVCONTAINER_PATH" 2>/dev/null || true
}

# Cleanup on exit
_cleanup() {
    stop_dworm
}

# Setup trap for cleanup
setup_cleanup() {
    trap _cleanup EXIT
}

# Run a test script and track result
run_test() {
    local test_script="$1"
    local test_path
    test_path="$(dirname "${BASH_SOURCE[1]}")/$test_script"

    if [[ ! -x "$test_path" ]]; then
        log_error "Test script not found or not executable: $test_script"
        ((FAILED++))
        return
    fi

    echo ""
    echo "=== Running: $test_script ==="

    if "$test_path"; then
        log_pass "$test_script"
        ((PASSED++))
    elif [[ $? -eq 77 ]]; then
        log_skip "$test_script"
        ((SKIPPED++))
    else
        log_fail "$test_script"
        ((FAILED++))
    fi
}
