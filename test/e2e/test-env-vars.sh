#!/bin/bash
# Test environment variable forwarding

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib/common.sh
source "$SCRIPT_DIR/lib/common.sh"
# shellcheck source=lib/skip.sh
source "$SCRIPT_DIR/lib/skip.sh"

require_docker
setup_cleanup

log_info "Testing environment variable forwarding..."

# Generate unique test value
TEST_VAR="dworm_test_value_$$"

# Start dworm with custom env var
log_info "Starting dworm with TEST_VAR=$TEST_VAR..."
start_dworm "-e TEST_VAR=$TEST_VAR"

# Get container ID
CONTAINER_ID=$(get_container_id)
if [[ -z "$CONTAINER_ID" ]]; then
    log_error "Container not found"
    exit 1
fi

# Check env var in container
log_info "Checking environment variable in container..."
RESULT=$(docker exec "$CONTAINER_ID" printenv TEST_VAR 2>/dev/null || true)

if [[ "$RESULT" == "$TEST_VAR" ]]; then
    log_pass "Environment variable forwarding works correctly"
    exit 0
else
    log_fail "Environment variable not found. Expected: $TEST_VAR, Got: $RESULT"
    exit 1
fi
