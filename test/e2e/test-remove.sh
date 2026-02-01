#!/bin/bash
# Test remove command

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib/common.sh
source "$SCRIPT_DIR/lib/common.sh"
# shellcheck source=lib/skip.sh
source "$SCRIPT_DIR/lib/skip.sh"

require_docker

# Custom cleanup - don't use setup_cleanup since we're testing removal
trap '(cd "$DEVCONTAINER_PATH" && "$DWORM" down) 2>/dev/null || true; docker rm -f $(docker ps -aq --filter "label=devcontainer.local_folder=$DEVCONTAINER_PATH") 2>/dev/null || true' EXIT

log_info "Testing remove command..."

# Start dworm
start_dworm

# Verify container exists
CONTAINER_ID=$(get_container_id)
if [[ -z "$CONTAINER_ID" ]]; then
    log_error "Container not found after start"
    exit 1
fi

# Get image ID for later verification
IMAGE_ID=$(docker inspect --format '{{.Image}}' "$CONTAINER_ID")

# Stop dworm process
if [[ -n "${DWORM_PID:-}" ]]; then
    kill "$DWORM_PID" 2>/dev/null || true
    wait "$DWORM_PID" 2>/dev/null || true
fi

# Run remove
log_info "Running dworm remove --force..."
(cd "$DEVCONTAINER_PATH" && "$DWORM" remove --force)

# Verify container is gone
REMAINING=$(docker ps -aq --filter "label=devcontainer.local_folder=$DEVCONTAINER_PATH")
if [[ -n "$REMAINING" ]]; then
    log_error "Container still exists after remove"
    exit 1
fi

# Verify image is gone
if docker image inspect "$IMAGE_ID" >/dev/null 2>&1; then
    log_error "Image still exists after remove"
    exit 1
fi

log_pass "Remove command works correctly"
