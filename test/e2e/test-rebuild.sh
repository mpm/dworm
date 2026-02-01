#!/bin/bash
# Test rebuild command

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib/common.sh
source "$SCRIPT_DIR/lib/common.sh"
# shellcheck source=lib/skip.sh
source "$SCRIPT_DIR/lib/skip.sh"

require_docker
setup_cleanup

log_info "Testing rebuild command..."

# Start dworm to create initial container
start_dworm

# Get original container ID
ORIGINAL_ID=$(get_container_id)
if [[ -z "$ORIGINAL_ID" ]]; then
    log_error "Container not found after start"
    exit 1
fi

# Stop dworm process
if [[ -n "${DWORM_PID:-}" ]]; then
    kill "$DWORM_PID" 2>/dev/null || true
    wait "$DWORM_PID" 2>/dev/null || true
fi

# Run rebuild
log_info "Running dworm rebuild..."
(cd "$DEVCONTAINER_PATH" && "$DWORM" rebuild)

# Get new container ID
NEW_ID=$(get_container_id)
if [[ -z "$NEW_ID" ]]; then
    log_error "No container found after rebuild"
    exit 1
fi

# Verify it's a different container
if [[ "$ORIGINAL_ID" == "$NEW_ID" ]]; then
    log_error "Container ID unchanged after rebuild"
    exit 1
fi

log_pass "Rebuild command works correctly"
