#!/bin/bash
# Test git credential forwarding

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib/common.sh
source "$SCRIPT_DIR/lib/common.sh"
# shellcheck source=lib/skip.sh
source "$SCRIPT_DIR/lib/skip.sh"

require_docker
require_git_creds
setup_cleanup

log_info "Testing git credential forwarding..."

# Start dworm
start_dworm

# Get container ID
CONTAINER_ID=$(get_container_id)
if [[ -z "$CONTAINER_ID" ]]; then
    log_error "Container not found"
    exit 1
fi

# Check credential helper is configured in container
log_info "Checking git credential helper configuration..."
HELPER=$(docker exec "$CONTAINER_ID" git config --get credential.helper 2>/dev/null || true)

if [[ -z "$HELPER" ]]; then
    log_fail "Git credential helper not configured in container"
    exit 1
fi

log_info "Credential helper: $HELPER"

# Verify the helper script exists
HELPER_PATH="/tmp/dworm-git-credential"
if ! docker exec "$CONTAINER_ID" test -x "$HELPER_PATH"; then
    log_fail "Git credential helper script not found or not executable: $HELPER_PATH"
    exit 1
fi

# Verify the credential socket exists
SOCKET_PATH="/tmp/dworm-git-credential.sock"
if ! docker exec "$CONTAINER_ID" test -S "$SOCKET_PATH"; then
    log_fail "Git credential socket not found: $SOCKET_PATH"
    exit 1
fi

log_pass "Git credential forwarding configured correctly"
# Note: Actually testing credential retrieval would require a private repo
# and valid credentials, which we can't easily test in CI
exit 0
