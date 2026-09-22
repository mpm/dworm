#!/bin/bash
# Single-host installation, synthetic credentials, and replacement of a live endpoint.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "$SCRIPT_DIR/lib/common.sh"
source "$SCRIPT_DIR/lib/skip.sh"
require_docker

DEVCONTAINER_PATH=$(mktemp -d)
cp -R "$PROJECT_ROOT/.devcontainer" "$DEVCONTAINER_PATH/.devcontainer"
mkdir "$DEVCONTAINER_PATH/bin"
cp "$DWORM" "$DEVCONTAINER_PATH/bin/dworm"
DWORM="$DEVCONTAINER_PATH/bin/dworm"
container_id=""
cleanup() {
    stop_dworm
    if [[ -n "$container_id" ]]; then docker rm -f "$container_id" >/dev/null; fi
    rm -rf "$DEVCONTAINER_PATH"
}
trap cleanup EXIT

# Override host credential configuration with disposable values only.
export GIT_CONFIG_COUNT=2
export GIT_CONFIG_KEY_0=credential.helper GIT_CONFIG_VALUE_0=""
export GIT_CONFIG_KEY_1=credential.helper
export GIT_CONFIG_VALUE_1='!f() { printf "username=fixture\npassword=fixture-secret\n"; }; f'
(cd "$DEVCONTAINER_PATH" && "$DWORM" up --daemon) > "$DEVCONTAINER_PATH/dworm.log" 2>&1 &
DWORM_PID=$!
for ((i=0; i<60; i++)); do
    container_id=$(docker ps -q --filter "label=devcontainer.local_folder=$DEVCONTAINER_PATH")
    if [[ -n "$container_id" ]] && docker exec "$container_id" test -S /tmp/dworm-git-credential.sock; then break; fi
    sleep 1
done
test -n "$container_id"
credentials=$(printf 'protocol=https\nhost=fixture.invalid\n\n' | docker exec -i "$container_id" /tmp/dworm-git-credential get)
[[ "$credentials" == *"password=fixture-secret"* ]]

# Atomically publish a second copy while the original inode is executing.
# This also preserves the fixed helper/launcher destination.
docker exec "$container_id" sh -c 'cp /tmp/dworm_endpoint /tmp/dworm_endpoint.next && mv -f /tmp/dworm_endpoint.next /tmp/dworm_endpoint'
credentials=$(printf 'protocol=https\nhost=fixture.invalid\n\n' | docker exec -i "$container_id" /tmp/dworm-git-credential get)
[[ "$credentials" == *"password=fixture-secret"* ]]

docker exec -d "$container_id" python3 -m http.server 18081
for ((i=0; i<30; i++)); do
    if curl -fsS http://127.0.0.1:18081/ >/dev/null; then
        log_pass "Single-binary install forwards ports and credentials after live endpoint replacement"
        exit 0
    fi
    sleep 1
done
cat "$DEVCONTAINER_PATH/dworm.log"
exit 1
