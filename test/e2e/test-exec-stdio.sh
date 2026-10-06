#!/bin/bash
# Test dworm exec stdio passthrough, exit codes, and working directories

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib/common.sh
source "$SCRIPT_DIR/lib/common.sh"
# shellcheck source=lib/skip.sh
source "$SCRIPT_DIR/lib/skip.sh"

require_docker
setup_cleanup

EXEC_FLAGS=()

dworm_exec() {
    (cd "$DEVCONTAINER_PATH" && "$DWORM" exec "${EXEC_FLAGS[@]}" "$@")
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

    log_info "[$label] Checking working directories..."
    out=$(dworm_exec -- pwd </dev/null)
    if [[ "$out" != "/home/developer/workspace" ]]; then
        log_fail "[$label] default working directory = $out, want the workspace folder"
        return 1
    fi
    out=$(dworm_exec -w /tmp -- pwd </dev/null)
    if [[ "$out" != "/tmp" ]]; then
        log_fail "[$label] --workdir /tmp gave $out"
        return 1
    fi

    log_info "[$label] Checking stdin EOF reaches the child..."
    out=$(printf 'x' | timeout 20 bash -c "cd '$DEVCONTAINER_PATH' && '$DWORM' exec ${EXEC_FLAGS[*]} -- sh -c 'cat >/dev/null; echo eof'")
    if [[ "$out" != "eof" ]]; then
        log_fail "[$label] child did not observe stdin EOF: $out"
        return 1
    fi
}

start_dworm
check_exec "via the instance"

# --no-bridge uses plain docker exec.
EXEC_FLAGS=(--no-bridge)
check_exec "--no-bridge"

log_pass "dworm exec passes stdio and exit codes through"
