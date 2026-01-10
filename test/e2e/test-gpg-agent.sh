#!/bin/bash
# Test GPG agent forwarding

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib/common.sh
source "$SCRIPT_DIR/lib/common.sh"
# shellcheck source=lib/skip.sh
source "$SCRIPT_DIR/lib/skip.sh"

require_docker
require_gpg_agent
setup_cleanup

log_info "Testing GPG agent forwarding..."

# Get a key fingerprint from host to verify later
HOST_KEYS=$(gpg --list-secret-keys --keyid-format LONG 2>/dev/null || true)
if [[ -z "$HOST_KEYS" ]]; then
    skip_test "No GPG secret keys found on host"
fi

# Extract first key ID
HOST_KEY_ID=$(echo "$HOST_KEYS" | grep -m1 "^sec" | awk '{print $2}' | cut -d'/' -f2)
log_info "Host GPG key ID: $HOST_KEY_ID"

# Start dworm
start_dworm

# Get container ID
CONTAINER_ID=$(get_container_id)
if [[ -z "$CONTAINER_ID" ]]; then
    log_error "Container not found"
    exit 1
fi

# Install GPG in container if not present
log_info "Ensuring GPG is installed in container..."
docker exec "$CONTAINER_ID" bash -c 'command -v gpg &>/dev/null || (apt-get update -qq && apt-get install -y -qq gnupg)' 2>/dev/null

# List keys in container - should see host's keys via forwarding
log_info "Checking for GPG keys in container..."
CONTAINER_KEYS=$(docker exec "$CONTAINER_ID" gpg --list-secret-keys --keyid-format LONG 2>/dev/null || true)

if echo "$CONTAINER_KEYS" | grep -q "$HOST_KEY_ID"; then
    log_pass "GPG agent forwarding works - host key visible in container"
    exit 0
else
    log_fail "GPG key not visible in container. Expected to find: $HOST_KEY_ID"
    log_info "Container keys: $CONTAINER_KEYS"
    exit 1
fi
