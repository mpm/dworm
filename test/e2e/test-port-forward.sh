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
docker exec -d "$CONTAINER_ID" /home/developer/test-server.sh 8080

# Wait for port to be forwarded
log_info "Waiting for port forwarding..."
sleep 5

# Test connection
log_info "Testing connection to localhost:8080..."
RESPONSE=$(curl -s --max-time 10 http://localhost:8080 2>/dev/null || true)

if [[ "$RESPONSE" == *"dworm test server"* ]]; then
    log_pass "Port forwarding works correctly"
    exit 0
else
    log_fail "Port forwarding failed. Response: $RESPONSE"
    exit 1
fi
