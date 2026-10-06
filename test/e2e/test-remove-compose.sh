#!/bin/bash
# Test remove on a Compose-based devcontainer: all services, the built images,
# and the project network are removed; volumes only with --volumes; pulled
# images are kept.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib/common.sh
source "$SCRIPT_DIR/lib/common.sh"
# shellcheck source=lib/skip.sh
source "$SCRIPT_DIR/lib/skip.sh"

require_docker
if ! docker compose version &>/dev/null; then
    skip_test "docker compose not available"
fi

DEVCONTAINER_PATH=$(mktemp -d)
mkdir "$DEVCONTAINER_PATH/.devcontainer"
cat > "$DEVCONTAINER_PATH/.devcontainer/devcontainer.json" <<'EOF'
{
  "name": "dworm-compose-test",
  "dockerComposeFile": "compose.yaml",
  "service": "app",
  "workspaceFolder": "/workspace",
  "remoteUser": "developer"
}
EOF
cat > "$DEVCONTAINER_PATH/.devcontainer/compose.yaml" <<'EOF'
services:
  app:
    build: .
    init: true
    command: sleep infinity
    volumes:
      - ..:/workspace
  sidecar:
    image: debian:bookworm-slim
    init: true
    command: sleep infinity
    volumes:
      - data:/data
volumes:
  data:
EOF
cat > "$DEVCONTAINER_PATH/.devcontainer/Dockerfile" <<'EOF'
FROM debian:bookworm-slim
# A non-root user whose UID differs from the host's makes the devcontainer CLI
# build a derived vsc-…-uid image on top of the Compose-built image.
RUN useradd -m -u 1999 developer
EOF

PROJECT=""
cleanup_remove_compose() {
    (cd "$DEVCONTAINER_PATH" && "$DWORM" stop) >/dev/null 2>&1 || true
    if [[ -n "$PROJECT" ]]; then
        local filter="label=com.docker.compose.project=$PROJECT"
        docker ps -aq --filter "$filter" | xargs -r docker rm -f -v >/dev/null 2>&1 || true
        docker images -q --filter "$filter" | sort -u | xargs -r docker rmi -f >/dev/null 2>&1 || true
        docker network ls -q --filter "$filter" | xargs -r docker network rm >/dev/null 2>&1 || true
        docker volume ls -q --filter "$filter" | xargs -r docker volume rm >/dev/null 2>&1 || true
    fi
    rm -rf "$DEVCONTAINER_PATH"
}
trap cleanup_remove_compose EXIT

# Start the instance and the Compose project; record what it created.
start_compose() {
    log_info "Starting Compose devcontainer..."
    (cd "$DEVCONTAINER_PATH" && "$DWORM" up -d) >/dev/null 2>&1
    local app
    app=$(docker ps -q --filter "label=devcontainer.local_folder=$DEVCONTAINER_PATH" | head -1)
    PROJECT=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project"}}' "$app")
    UID_IMAGE=$(docker inspect --format '{{.Image}}' "$app")
    APP_IMAGE="$PROJECT-app"
    if [[ $(docker ps -q --filter "label=com.docker.compose.project=$PROJECT" | wc -l) -ne 2 ]]; then
        log_error "Expected 2 running services in project $PROJECT"
        exit 1
    fi
    docker image inspect "$APP_IMAGE" >/dev/null
    docker network inspect "${PROJECT}_default" >/dev/null
    docker volume inspect "${PROJECT}_data" >/dev/null
}

# Verify that containers, built images, and the network are gone.
check_removed() {
    if [[ -n $(docker ps -aq --filter "label=com.docker.compose.project=$PROJECT") ]]; then
        log_error "Containers of project $PROJECT still exist"
        exit 1
    fi
    for image in "$UID_IMAGE" "$APP_IMAGE"; do
        if docker image inspect "$image" >/dev/null 2>&1; then
            log_error "Image $image still exists"
            exit 1
        fi
    done
    if docker network inspect "${PROJECT}_default" >/dev/null 2>&1; then
        log_error "Network ${PROJECT}_default still exists"
        exit 1
    fi
    if ! docker image inspect debian:bookworm-slim >/dev/null 2>&1; then
        log_error "Pulled image debian:bookworm-slim was removed"
        exit 1
    fi
}

log_info "Testing remove of a Compose devcontainer..."
start_compose

log_info "Running dworm remove --force (keeps volumes)..."
OUTPUT=$(cd "$DEVCONTAINER_PATH" && "$DWORM" remove --force 2>&1)
echo "$OUTPUT"
check_removed
if ! docker volume inspect "${PROJECT}_data" >/dev/null 2>&1; then
    log_error "Volume ${PROJECT}_data was removed without --volumes"
    exit 1
fi
for expected in "Removed containers:" "${PROJECT}-sidecar-1" "$APP_IMAGE" "Removed networks: ${PROJECT}_default" "Kept volumes" "${PROJECT}_data"; do
    if [[ "$OUTPUT" != *"$expected"* ]]; then
        log_error "Output lacks '$expected'"
        exit 1
    fi
done
log_pass "Compose project removed, volume kept"

start_compose

log_info "Running dworm remove --force --volumes..."
OUTPUT=$(cd "$DEVCONTAINER_PATH" && "$DWORM" remove --force --volumes 2>&1)
echo "$OUTPUT"
check_removed
if docker volume inspect "${PROJECT}_data" >/dev/null 2>&1; then
    log_error "Volume ${PROJECT}_data still exists after --volumes"
    exit 1
fi
if [[ "$OUTPUT" != *"Removed volumes: ${PROJECT}_data"* ]]; then
    log_error "Output lacks the removed volume"
    exit 1
fi

log_pass "Remove of a Compose devcontainer works correctly"
