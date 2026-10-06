#!/bin/bash
# Test the workspace instance: dworm up -d, a shell on a PTY that leaves the
# instance running, dworm logs, TTY exec cleanup, dworm stop/down, and a cold
# dworm exec that starts everything

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib/common.sh
source "$SCRIPT_DIR/lib/common.sh"
# shellcheck source=lib/skip.sh
source "$SCRIPT_DIR/lib/skip.sh"

require_docker
if ! command -v python3 &>/dev/null || ! command -v script &>/dev/null; then
    skip_test "python3 and script are needed to drive terminals"
fi
setup_cleanup

WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"; _cleanup' EXIT

dworm() {
    (cd "$DEVCONTAINER_PATH" && "$DWORM" "$@")
}

container_has() {
    docker exec "$(get_container_id)" pgrep -f "$1" >/dev/null 2>&1
}

wait_for() {
    local seconds="$1" i
    shift
    for ((i = 0; i < seconds * 10; i++)); do
        "$@" && return 0
        sleep 0.1
    done
    return 1
}

# Runs a command on a PTY, types lines into it, and prints its output and
# exit status.
cat >"$WORK_DIR/pty_drive.py" <<'EOF'
import os, pty, select, sys, time
pid, fd = pty.fork()
if pid == 0:
    os.chdir(sys.argv[1])
    os.execv(sys.argv[2], sys.argv[2:])
out = b""
def read_for(seconds):
    global out
    end = time.time() + seconds
    while time.time() < end:
        if select.select([fd], [], [], 0.1)[0]:
            try:
                out += os.read(fd, 65536)
            except OSError:
                return
for line in os.environ["PTY_INPUT"].split("\n"):
    read_for(3)
    os.write(fd, line.encode() + b"\r")
read_for(5)
_, status = os.waitpid(pid, 0)
sys.stdout.write(out.decode(errors="replace"))
print("\nexit=%d" % os.waitstatus_to_exitcode(status))
EOF

dworm stop >/dev/null 2>&1 || true

log_info "dworm up -d starts a detached instance and returns..."
out=$(dworm up -d </dev/null)
if [[ "$out" != "dworm instance ready:"* ]]; then
    log_fail "unexpected dworm up -d output: $out"
    exit 1
fi
PID=$(status_field up.pid)
if [[ "$(status_field up.mode)" != '"detached"' || "$(status_field up.state)" != '"ready"' ]] || ! kill -0 "$PID"; then
    log_fail "no detached, ready instance after dworm up -d (pid $PID, mode $(status_field up.mode))"
    exit 1
fi

log_info "A second dworm up -d returns immediately..."
start=$(date +%s%N)
dworm up -d </dev/null >/dev/null
elapsed_ms=$((($(date +%s%N) - start) / 1000000))
if [[ "$elapsed_ms" -gt 2000 || "$(status_field up.pid)" != "$PID" ]]; then
    log_fail "second dworm up -d took ${elapsed_ms}ms or started another instance"
    exit 1
fi

log_info "dworm shell runs on a PTY and leaves the instance running..."
out=$(PTY_INPUT=$'echo shell-$((6*7)) $(tty); stty size\nexit 4' python3 "$WORK_DIR/pty_drive.py" "$DEVCONTAINER_PATH" "$DWORM" shell)
if [[ "$out" != *"shell-42 /dev/pts/"* || "$out" != *"Ctrl+G"* || "$out" != *"exit=4" ]]; then
    log_fail "dworm shell session did not work as expected:"
    printf '%s\n' "$out" | tail -20
    exit 1
fi
if [[ "$(status_field up.running)" != "true" || "$(status_field up.pid)" != "$PID" ]]; then
    log_fail "the instance did not survive leaving the shell"
    exit 1
fi

log_info "dworm logs -f --json sees exec_started and exec_exited..."
dworm logs -f --json -n 0 >"$WORK_DIR/events.json" 2>/dev/null &
LOGS_PID=$!
wait_for 10 test -s "$WORK_DIR/events.json" || { log_fail "dworm logs -f wrote nothing"; exit 1; }
dworm exec -- true </dev/null
check_events() {
    python3 - "$WORK_DIR/events.json" <<'EOF'
import json, sys
events = [json.loads(line) for line in open(sys.argv[1]) if line.strip()]
started = {e["id"] for e in events if e["type"] == "exec_started" and e["argv"] == ["true"] and e["tty"] is False}
exited = {e["id"] for e in events if e["type"] == "exec_exited" and e["code"] == 0 and e["signal"] == ""}
sys.exit(0 if started & exited else 1)
EOF
}
if ! wait_for 10 check_events; then
    log_fail "exec events missing from dworm logs -f --json:"
    cat "$WORK_DIR/events.json"
    exit 1
fi
kill "$LOGS_PID" 2>/dev/null || true
wait "$LOGS_PID" 2>/dev/null || true
# The history holds log events, including the host's line about the exec.
if ! dworm logs -n 50 | grep -q '\[host\] \[exec [0-9a-f]*\] Exited with code 0'; then
    log_fail "dworm logs did not show the exec in its history"
    exit 1
fi

log_info "TTY dworm exec: a SIGKILLed client leaves no process behind..."
(cd "$DEVCONTAINER_PATH" && script -qec "'$DWORM' exec -- sh -c 'trap \"\" HUP TERM; sleep 4848'" /dev/null >/dev/null 2>&1 &)
wait_for 15 container_has "sleep 4848" || { log_fail "TTY exec did not start"; exit 1; }
CLIENT=$(pgrep -f "^$DWORM exec -- sh -c" | head -1)
kill -KILL "$CLIENT"
if ! wait_for 15 eval '! container_has "sleep 4848"'; then
    log_fail "process survived its SIGKILLed TTY dworm exec client"
    exit 1
fi

log_info "dworm stop stops the instance and keeps the container..."
dworm stop >/dev/null 2>&1
# The lock is released as the process exits; it may take a moment to be reaped.
if ! wait_for 5 eval '! kill -0 "$PID" 2>/dev/null' || [[ "$(status_field up.running)" != "false" || "$(status_field container.running)" != "true" ]]; then
    log_fail "after dworm stop: instance pid $PID alive or status wrong (up.running $(status_field up.running), container.running $(status_field container.running))"
    exit 1
fi

log_info "dworm exec from cold starts the container and the instance..."
dworm down >/dev/null 2>&1
if [[ "$(status_field container.running)" != "false" ]]; then
    log_fail "container still running after dworm down"
    exit 1
fi
out=$(dworm exec -- echo cold-start </dev/null 2>/dev/null)
if [[ "$out" != "cold-start" || "$(status_field up.running)" != "true" || "$(status_field container.running)" != "true" ]]; then
    log_fail "cold dworm exec printed '$out' (up.running $(status_field up.running), container.running $(status_field container.running))"
    exit 1
fi

log_info "dworm down stops the instance and the container..."
PID=$(status_field up.pid)
dworm down >/dev/null 2>&1
if ! wait_for 5 eval '! kill -0 "$PID" 2>/dev/null' || [[ "$(status_field up.running)" != "false" || "$(status_field container.running)" != "false" ]]; then
    log_fail "after dworm down: instance or container still running"
    exit 1
fi

log_pass "The workspace instance outlives its clients and is controlled by stop/down"
