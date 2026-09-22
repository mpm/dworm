#!/bin/bash
# Project configuration, host execution, refresh, and independent exec invocations.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "$SCRIPT_DIR/lib/common.sh"
source "$SCRIPT_DIR/lib/skip.sh"
require_docker

DEVCONTAINER_PATH=$(mktemp -d)
cp -R "$PROJECT_ROOT/.devcontainer" "$DEVCONTAINER_PATH/.devcontainer"
cleanup_project_env() {
    local container_id
    container_id=$(docker ps -aq --filter "label=devcontainer.local_folder=$DEVCONTAINER_PATH")
    stop_dworm
    if [[ -n "$container_id" ]]; then docker rm -f "$container_id" >/dev/null; fi
    rm -rf "$DEVCONTAINER_PATH"
}
trap cleanup_project_env EXIT

cat > "$DEVCONTAINER_PATH/.dworm.config" <<'EOF'
bind = "127.0.0.1"
[host_env]
command = ["bash", "./generate-env.sh"]
refresh_interval = "1s"
timeout = "5s"
EOF
cat > "$DEVCONTAINER_PATH/.dworm.env" <<'EOF'
TOKEN=static-fallback
STATIC=from-file
PIN=static
EOF
cat > "$DEVCONTAINER_PATH/generate-env.sh" <<'EOF'
#!/bin/bash
set -eu
if [[ ! -f initial-run ]]; then
    # The first execution must happen before this workspace has a container.
    test -z "$(docker ps -aq --filter "label=devcontainer.local_folder=$PWD")"
    touch initial-run
fi
cat generated.env
EOF
printf 'TOKEN=first\nPIN=generated\nREMOVED=present\n' > "$DEVCONTAINER_PATH/generated.env"

log_info "Testing project environment and periodic refresh..."
(cd "$DEVCONTAINER_PATH" && "$DWORM" up --daemon -e PIN=cli) > "$DEVCONTAINER_PATH/dworm.log" 2>&1 &
DWORM_PID=$!

read_values() {
    (cd "$DEVCONTAINER_PATH" && "$DWORM" exec -- sh -c 'printf "%s|%s|%s|%s" "$TOKEN" "$STATIC" "$PIN" "${REMOVED-unset}"') 2>/dev/null
}
wait_values() {
    local expected="$1" result=""
    for ((i=0; i<60; i++)); do
        result=$(read_values || true)
        if [[ "$result" == "$expected" ]]; then return 0; fi
        if ! kill -0 "$DWORM_PID" 2>/dev/null; then break; fi
        sleep 1
    done
    log_fail "Expected '$expected', got '$result'"
    cat "$DEVCONTAINER_PATH/dworm.log"
    return 1
}

wait_values 'first|from-file|cli|present'
test -f "$DEVCONTAINER_PATH/initial-run"

# Publish source changes atomically so the script never reads a partial fixture.
printf 'TOKEN=second\nPIN=generated-again\n' > "$DEVCONTAINER_PATH/next.env"
mv "$DEVCONTAINER_PATH/next.env" "$DEVCONTAINER_PATH/generated.env"
wait_values 'second|from-file|cli|unset'

# Invalid output must not replace the last successful snapshot.
printf 'invalid output\n' > "$DEVCONTAINER_PATH/next.env"
mv "$DEVCONTAINER_PATH/next.env" "$DEVCONTAINER_PATH/generated.env"
sleep 3
wait_values 'second|from-file|cli|unset'

# Omitting TOKEN restores the static fallback on the next successful execution.
printf 'PIN=generated\n' > "$DEVCONTAINER_PATH/next.env"
mv "$DEVCONTAINER_PATH/next.env" "$DEVCONTAINER_PATH/generated.env"
wait_values 'static-fallback|from-file|cli|unset'

override=$(cd "$DEVCONTAINER_PATH" && "$DWORM" exec -e TOKEN=one-command -- printenv TOKEN)
test "$override" = one-command
wait_values 'static-fallback|from-file|cli|unset'

# A stopped host bridge leaves a usable last snapshot.
kill "$DWORM_PID"
wait "$DWORM_PID"
DWORM_PID=""
test "$(read_values)" = 'static-fallback|from-file|cli|unset'
log_pass "Host startup, dotenv, CLI precedence, refresh, removals, and failure retention work"
