#!/bin/bash
# Test dworm up --daemon lifecycle: single instance, state file, signal and
# bridge-failure exit codes, plain log line endings

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib/common.sh
source "$SCRIPT_DIR/lib/common.sh"
# shellcheck source=lib/skip.sh
source "$SCRIPT_DIR/lib/skip.sh"

require_docker
setup_cleanup

LOG_DIR=$(mktemp -d)
trap 'rm -rf "$LOG_DIR"; _cleanup' EXIT

RUNTIME_DIR="${XDG_RUNTIME_DIR:+$XDG_RUNTIME_DIR/dworm}"
RUNTIME_DIR="${RUNTIME_DIR:-${TMPDIR:-/tmp}/dworm-$(id -u)}"
HASH=$(printf '%s' "$DEVCONTAINER_PATH" | sha256sum | cut -c1-12)
STATE_FILE="$RUNTIME_DIR/$HASH.json"

start_daemon() {
    cd "$DEVCONTAINER_PATH"
    "$DWORM" up --daemon 2>"$1" &
    DWORM_PID=$!
    cd - >/dev/null
    local i
    for ((i = 0; i < 120; i++)); do
        if [[ -f "$STATE_FILE" ]] && grep -q '"endpoint_connected": true' "$STATE_FILE"; then
            return 0
        fi
        if ! kill -0 "$DWORM_PID" 2>/dev/null; then
            log_fail "dworm up exited during startup"
            cat "$1"
            return 1
        fi
        sleep 1
    done
    log_fail "Timed out waiting for the state file"
    return 1
}

wait_exit() {
    local status=0
    timeout "$2" tail --pid="$1" -f /dev/null || return 124
    wait "$1" || status=$?
    return "$status"
}

log_info "Starting daemon..."
start_daemon "$LOG_DIR/first.log"

log_info "Checking state file..."
if ! grep -q "\"pid\": $DWORM_PID" "$STATE_FILE" || ! grep -q '"workspace_folder": "/home/developer/workspace"' "$STATE_FILE"; then
    log_fail "State file does not describe the daemon:"
    cat "$STATE_FILE"
    exit 1
fi
if [[ "$(stat -c %a "$RUNTIME_DIR")" != "700" ]]; then
    log_fail "Runtime directory mode is $(stat -c %a "$RUNTIME_DIR"), want 700"
    exit 1
fi

log_info "Checking that a second dworm up fails fast..."
status=0
(cd "$DEVCONTAINER_PATH" && timeout 20 "$DWORM" up --daemon 2>"$LOG_DIR/second.log") || status=$?
if [[ "$status" -ne 3 ]]; then
    log_fail "Second dworm up exited with $status, want 3"
    cat "$LOG_DIR/second.log"
    exit 1
fi
if ! grep -q "already running" "$LOG_DIR/second.log"; then
    log_fail "Second dworm up did not explain the failure"
    exit 1
fi

log_info "Checking that SIGTERM exits 0 and keeps the container..."
CONTAINER_ID=$(get_container_id)
kill -TERM "$DWORM_PID"
status=0
wait_exit "$DWORM_PID" 30 || status=$?
DWORM_PID=
if [[ "$status" -ne 0 ]]; then
    log_fail "dworm up exited with $status after SIGTERM, want 0"
    cat "$LOG_DIR/first.log"
    exit 1
fi
if [[ "$(docker inspect -f '{{.State.Running}}' "$CONTAINER_ID")" != "true" ]]; then
    log_fail "Container was stopped by SIGTERM"
    exit 1
fi
if [[ -e "$STATE_FILE" ]]; then
    log_fail "State file remains after shutdown"
    exit 1
fi
if grep -q $'\r' "$LOG_DIR/first.log"; then
    log_fail "Non-terminal log output contains carriage returns"
    exit 1
fi

log_info "Checking that a broken bridge exits non-zero..."
start_daemon "$LOG_DIR/third.log"
docker restart "$CONTAINER_ID" >/dev/null
status=0
wait_exit "$DWORM_PID" 60 || status=$?
DWORM_PID=
if [[ "$status" -eq 0 || "$status" -eq 124 ]]; then
    log_fail "dworm up exit status after container restart = $status, want non-zero exit"
    cat "$LOG_DIR/third.log"
    exit 1
fi

log_pass "Daemon lifecycle behaves as expected"
