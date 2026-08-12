#!/bin/bash
# Test port forwarding functionality

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib/common.sh
source "$SCRIPT_DIR/lib/common.sh"
# shellcheck source=lib/skip.sh
source "$SCRIPT_DIR/lib/skip.sh"

require_docker
setup_cleanup

log_info "Testing port forwarding..."

wait_for_response() {
    local attempts="${1:-15}"
    local i
    local response
    for ((i = 0; i < attempts; i++)); do
        response=$(curl -s --max-time 2 http://localhost:8080 2>/dev/null || true)
        if [[ "$response" == *"dworm test server"* ]]; then
            return 0
        fi
        sleep 1
    done
    return 1
}

start_test_server() {
    docker exec -d "$CONTAINER_ID" /home/developer/test-server.sh 8080 >/dev/null
}

stop_test_server() {
    docker exec "$CONTAINER_ID" pkill -f "python3 -m http.server 8080" >/dev/null 2>&1 || true
}

# Start dworm
start_dworm

# Get container ID
CONTAINER_ID=$(get_container_id)
if [[ -z "$CONTAINER_ID" ]]; then
    log_error "Container not found"
    exit 1
fi

# Start test server in container
log_info "Starting test server in container..."
start_test_server

# Wait for port to be forwarded
log_info "Waiting for port forwarding..."
if ! wait_for_response; then
    log_fail "Initial port forwarding request failed"
    exit 1
fi

# Exercise stream cleanup with repeated short connections.
log_info "Testing repeated short connections..."
for ((i = 0; i < 25; i++)); do
    if ! wait_for_response 1; then
        log_fail "Port forwarding failed on repeated request $i"
        exit 1
    fi
done

# Yamux sends keepalives every 30 seconds. Verify the session remains usable
# after crossing that boundary without tunnel traffic.
log_info "Testing forwarding after an idle keepalive interval..."
sleep 32
if ! wait_for_response 5; then
    log_fail "Port forwarding did not recover after an idle interval"
    exit 1
fi

# Restart quickly enough that the scanner may never observe the closed port.
log_info "Testing server restart within one scan interval..."
stop_test_server
start_test_server
if ! wait_for_response; then
    log_fail "Port forwarding failed after a quick server restart"
    exit 1
fi

# Leave the port closed long enough for its listener and active tunnels to be
# removed, then verify that a later update recreates forwarding.
log_info "Testing server restart beyond one scan interval..."
stop_test_server
sleep 3
if curl -fsS --max-time 2 http://localhost:8080 >/dev/null 2>&1; then
    log_fail "Stopped container port remained reachable"
    exit 1
fi
start_test_server
if ! wait_for_response; then
    log_fail "Port forwarding failed after a scanned server restart"
    exit 1
fi

log_pass "Port forwarding survives repeated connections, idle time, and restarts"
