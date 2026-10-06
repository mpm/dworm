#!/bin/bash
# Test dworm exec stdio passthrough and exit codes

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib/common.sh
source "$SCRIPT_DIR/lib/common.sh"
# shellcheck source=lib/skip.sh
source "$SCRIPT_DIR/lib/skip.sh"

require_docker
setup_cleanup

dworm_exec() {
    (cd "$DEVCONTAINER_PATH" && "$DWORM" exec "$@")
}

check_exec() {
    local label="$1"
    local out

    log_info "[$label] Checking stdin passthrough..."
    out=$(printf 'a\nb\n' | dworm_exec -- cat | od -An -c | tr -s ' ')
    if [[ "$out" != "$(printf 'a\nb\n' | od -An -c | tr -s ' ')" ]]; then
        log_fail "[$label] stdout was not exactly the piped input: $out"
        return 1
    fi

    log_info "[$label] Checking that diagnostics stay off stdout..."
    out=$(dworm_exec -- sh -c 'echo out; echo err >&2' 2>/dev/null)
    if [[ "$out" != "out" ]]; then
        log_fail "[$label] unexpected stdout: $out"
        return 1
    fi

    log_info "[$label] Checking exit code passthrough..."
    local status=0
    dworm_exec -- sh -c 'exit 7' </dev/null || status=$?
    if [[ "$status" -ne 7 ]]; then
        log_fail "[$label] exit status = $status, want 7"
        return 1
    fi

    log_info "[$label] Checking stdin EOF reaches the child..."
    out=$(printf 'x' | timeout 20 bash -c "cd '$DEVCONTAINER_PATH' && '$DWORM' exec -- sh -c 'cat >/dev/null; echo eof'")
    if [[ "$out" != "eof" ]]; then
        log_fail "[$label] child did not observe stdin EOF: $out"
        return 1
    fi
}

start_dworm
check_exec "with dworm up"

# Stop the bridge but keep the container: exec uses plain docker exec.
kill "$DWORM_PID" 2>/dev/null || true
wait "$DWORM_PID" 2>/dev/null || true
DWORM_PID=
check_exec "without dworm up"

log_pass "dworm exec passes stdio and exit codes through"
