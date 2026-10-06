#!/bin/bash
# Test dworm up --foreground lifecycle: single instance, state file, status --json,
# SIGTERM exit code, reconnect after docker restart, plain log line endings

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

start_daemon() {
    cd "$DEVCONTAINER_PATH"
    "$DWORM" up --foreground 2>"$1" &
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

log_info "Checking dworm status --json..."
for check in "up.running=true" "up.pid=$DWORM_PID" "up.endpoint_connected=true" \
    "container.running=true" "container.workspace_folder=\"/home/developer/workspace\""; do
    got=$(status_field "${check%%=*}")
    if [[ "$got" != "${check#*=}" ]]; then
        log_fail "status ${check%%=*} = $got, want ${check#*=}"
        exit 1
    fi
done

log_info "Checking that a second dworm up fails fast..."
status=0
(cd "$DEVCONTAINER_PATH" && timeout 20 "$DWORM" up --foreground 2>"$LOG_DIR/second.log") || status=$?
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
if [[ "$(status_field up.running)" != "false" ]]; then
    log_fail "status reports up.running after shutdown"
    exit 1
fi
if grep -q $'\r' "$LOG_DIR/first.log"; then
    log_fail "Non-terminal log output contains carriage returns"
    exit 1
fi

log_info "Checking that the instance reconnects after docker restart..."
start_daemon "$LOG_DIR/third.log"
docker restart "$CONTAINER_ID" >/dev/null
for ((i = 0; i < 60; i++)); do
    if [[ "$(state_field reconnects)" == "1" && "$(state_field state)" == '"ready"' ]]; then
        break
    fi
    if ! kill -0 "$DWORM_PID" 2>/dev/null; then
        log_fail "dworm up exited after the container restart"
        cat "$LOG_DIR/third.log"
        exit 1
    fi
    sleep 1
done
if [[ "$(state_field pid)" != "$DWORM_PID" || "$(state_field state)" != '"ready"' ]]; then
    log_fail "instance did not reconnect: pid $(state_field pid) (want $DWORM_PID), state $(state_field state), reconnects $(state_field reconnects)"
    cat "$LOG_DIR/third.log"
    exit 1
fi
if ! (cd "$DEVCONTAINER_PATH" && "$DWORM" exec -- true </dev/null); then
    log_fail "dworm exec failed after the reconnect"
    exit 1
fi

log_pass "Daemon lifecycle behaves as expected"
