#!/bin/bash
# Test SSH agent forwarding

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib/common.sh
source "$SCRIPT_DIR/lib/common.sh"
# shellcheck source=lib/skip.sh
source "$SCRIPT_DIR/lib/skip.sh"

require_docker
require_ssh_agent
setup_cleanup

log_info "Testing SSH agent forwarding..."

# Start dworm
start_dworm

# Get container ID
CONTAINER_ID=$(get_container_id)
if [[ -z "$CONTAINER_ID" ]]; then
    log_error "Container not found"
    exit 1
fi

# Check SSH_AUTH_SOCK is set in container
log_info "Checking SSH_AUTH_SOCK in container..."
SSH_SOCK=$(docker exec "$CONTAINER_ID" printenv SSH_AUTH_SOCK 2>/dev/null || true)

if [[ -z "$SSH_SOCK" ]]; then
    log_fail "SSH_AUTH_SOCK not set in container"
    exit 1
fi

log_info "SSH_AUTH_SOCK in container: $SSH_SOCK"

# Check if the socket exists
log_info "Checking if SSH agent socket exists..."
if ! docker exec "$CONTAINER_ID" test -S "$SSH_SOCK"; then
    log_fail "SSH agent socket does not exist: $SSH_SOCK"
    exit 1
fi

# Try to communicate with the agent
log_info "Testing SSH agent communication..."
# Install openssh-client if not present (for ssh-add)
docker exec "$CONTAINER_ID" bash -c 'command -v ssh-add &>/dev/null || (apt-get update -qq && apt-get install -y -qq openssh-client)' 2>/dev/null

if docker exec "$CONTAINER_ID" ssh-add -l &>/dev/null; then
    log_pass "SSH agent forwarding works - keys visible"
    exit 0
elif [[ $? -eq 1 ]]; then
    # Exit code 1 means agent is running but has no keys - still working
    log_pass "SSH agent forwarding works - agent connected (no keys)"
    exit 0
else
    log_fail "SSH agent communication failed"
    exit 1
fi
