#!/bin/bash
# Project-scoped host settings must reach Compose on both startup and rebuild.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "$SCRIPT_DIR/lib/common.sh"
source "$SCRIPT_DIR/lib/skip.sh"
require_docker

DEVCONTAINER_PATH=$(mktemp -d)
PROJECT_NAME="dworm-build-env-$$"
cp -R "$PROJECT_ROOT/.devcontainer" "$DEVCONTAINER_PATH/.devcontainer"
cleanup_devcontainer_env() {
    (cd "$DEVCONTAINER_PATH" && "$DWORM" remove --force) || true
    rm -rf "$DEVCONTAINER_PATH"
}
trap cleanup_devcontainer_env EXIT

cat > "$DEVCONTAINER_PATH/.devcontainer/devcontainer.json" <<'EOF'
{
  "name": "dworm-build-env-test",
  "dockerComposeFile": "compose.yaml",
  "service": "app",
  "remoteUser": "developer",
  "workspaceFolder": "/home/developer/workspace",
  "initializeCommand": ["sh", "-c", "test \"$DWORM_TEST_BUILD_ONLY\" = from-project"]
}
EOF
cat > "$DEVCONTAINER_PATH/.devcontainer/compose.yaml" <<EOF
name: $PROJECT_NAME
services:
  app:
    image: \${DWORM_TEST_IMAGE:?devcontainer_env must reach Compose}
    build: .
    command: sleep infinity
    volumes:
      - ..:/home/developer/workspace
EOF
cat > "$DEVCONTAINER_PATH/.dworm.config" <<EOF
[devcontainer_env]
BUILDX_BAKE_ENTITLEMENTS_FS = "0"
DWORM_TEST_BUILD_ONLY = "from-project"
DWORM_TEST_IMAGE = "$PROJECT_NAME:latest"
EOF

unset BUILDX_BAKE_ENTITLEMENTS_FS DWORM_TEST_BUILD_ONLY DWORM_TEST_IMAGE
log_info "Testing configured host environment with a fresh Compose build..."
(cd "$DEVCONTAINER_PATH" && "$DWORM" up -d)
original_id=$(get_container_id)
test -n "$original_id"
(cd "$DEVCONTAINER_PATH" && "$DWORM" exec -- sh -c 'test "${DWORM_TEST_BUILD_ONLY-unset}" = unset && test "${BUILDX_BAKE_ENTITLEMENTS_FS-unset}" = unset')

log_info "Testing rebuild with the same configuration and no command prefix..."
(cd "$DEVCONTAINER_PATH" && "$DWORM" rebuild && "$DWORM" up -d)
new_id=$(get_container_id)
test -n "$new_id"
test "$new_id" != "$original_id"
(cd "$DEVCONTAINER_PATH" && "$DWORM" exec -- sh -c 'test "${DWORM_TEST_BUILD_ONLY-unset}" = unset && test "${BUILDX_BAKE_ENTITLEMENTS_FS-unset}" = unset')
log_pass "Configured host settings reach startup/rebuild without leaking into container sessions"
