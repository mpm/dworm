#!/bin/bash
# Test exec over the dworm up bridge: raw socket clients, dworm exec via the
# socket, and process cleanup when callers disappear

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib/common.sh
source "$SCRIPT_DIR/lib/common.sh"
# shellcheck source=lib/skip.sh
source "$SCRIPT_DIR/lib/skip.sh"

require_docker
if ! command -v python3 &>/dev/null; then
    skip_test "python3 not installed (used as raw socket client)"
fi
setup_cleanup

WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"; _cleanup' EXIT

# Minimal raw-mode client: sends the header line, prints the reply line to
# stderr, copies stdin to the socket, half-closes, and copies the rest to stdout.
cat >"$WORK_DIR/raw_client.py" <<'EOF'
import json, socket, sys
sock = socket.socket(socket.AF_UNIX)
sock.connect(sys.argv[1])
request = {"version": 1, "argv": sys.argv[2:], "mode": "raw"}
sock.sendall(json.dumps(request).encode() + b"\n")
reply = b""
while not reply.endswith(b"\n"):
    chunk = sock.recv(1)
    if not chunk:
        sys.exit("connection closed before reply")
    reply += chunk
print(reply.decode().strip(), file=sys.stderr, flush=True)
if not json.loads(reply)["ok"]:
    sys.exit(1)
sock.sendall(sys.stdin.buffer.read())
sock.shutdown(socket.SHUT_WR)
while True:
    data = sock.recv(65536)
    if not data:
        break
    sys.stdout.buffer.write(data)
EOF

DAEMON_LOG="$WORK_DIR/daemon.log"
(cd "$DEVCONTAINER_PATH" && exec "$DWORM" up --foreground 2>"$DAEMON_LOG") &
DWORM_PID=$!

# The socket exists before the instance is ready; exec requests need "ready".
SOCKET=""
for ((i = 0; i < 120; i++)); do
    if [[ "$(status_field up.state)" == '"ready"' ]]; then
        SOCKET=$(status_field up.exec_socket | tr -d '"')
        break
    fi
    sleep 1
done
if [[ -z "$SOCKET" ]]; then
    log_fail "status --json never reported a ready instance with exec_socket"
    cat "$DAEMON_LOG"
    exit 1
fi
if [[ "$(stat -c %a "$SOCKET")" != "600" ]]; then
    log_fail "exec socket mode is $(stat -c %a "$SOCKET"), want 600"
    exit 1
fi
CONTAINER_ID=$(get_container_id)

container_has() {
    docker exec "$CONTAINER_ID" pgrep -f "$1" >/dev/null 2>&1
}

wait_gone() {
    local pattern="$1" seconds="$2" i
    for ((i = 0; i < seconds * 10; i++)); do
        container_has "$pattern" || return 0
        sleep 0.1
    done
    return 1
}

wait_present() {
    local pattern="$1" i
    for ((i = 0; i < 100; i++)); do
        container_has "$pattern" && return 0
        sleep 0.1
    done
    return 1
}

log_info "Raw mode: stdin/stdout passthrough..."
out=$(printf 'a\nb\n' | python3 "$WORK_DIR/raw_client.py" "$SOCKET" cat 2>/dev/null | od -An -c | tr -s ' ')
if [[ "$out" != "$(printf 'a\nb\n' | od -An -c | tr -s ' ')" ]]; then
    log_fail "raw mode output: $out"
    exit 1
fi

log_info "Raw mode: default working directory and stderr goes to the log..."
out=$(python3 "$WORK_DIR/raw_client.py" "$SOCKET" sh -c 'pwd; echo raw-stderr-marker >&2' </dev/null 2>/dev/null)
if [[ "$out" != "/home/developer/workspace" ]]; then
    log_fail "raw mode cwd: $out"
    exit 1
fi
sleep 0.5
if ! grep -q "raw-stderr-marker" "$DAEMON_LOG"; then
    log_fail "raw mode stderr did not reach the dworm up log"
    exit 1
fi

log_info "Raw mode: killed client leaves no process behind..."
# The client's stdin stays open (FIFO) so only the kill ends the session.
mkfifo "$WORK_DIR/stdin"
exec 3<>"$WORK_DIR/stdin"
python3 "$WORK_DIR/raw_client.py" "$SOCKET" sh -c 'trap "" TERM; sleep 4242' <"$WORK_DIR/stdin" >/dev/null 2>&1 &
CLIENT=$!
wait_present "sleep 4242" || { log_fail "raw exec did not start"; exit 1; }
kill -KILL "$CLIENT"
wait "$CLIENT" 2>/dev/null || true
if ! wait_gone "sleep 4242" 8; then
    log_fail "process survived its killed raw client (TERM ignored, needs SIGKILL after grace)"
    exit 1
fi

log_info "dworm exec uses the socket..."
out=$(printf 'via socket\n' | (cd "$DEVCONTAINER_PATH" && "$DWORM" exec -- cat))
if [[ "$out" != "via socket" ]] || ! grep -q '"cat" (framed mode)' "$DAEMON_LOG"; then
    log_fail "dworm exec did not run through the socket (output: $out)"
    exit 1
fi

log_info "dworm exec: SIGKILLed caller leaves no process behind..."
(cd "$DEVCONTAINER_PATH" && exec "$DWORM" exec -- sleep 4343 </dev/null >/dev/null 2>&1) &
EXEC_PID=$!
wait_present "sleep 4343" || { log_fail "dworm exec did not start"; exit 1; }
kill -KILL "$EXEC_PID"
if ! wait_gone "sleep 4343" 3; then
    log_fail "process survived its SIGKILLed dworm exec caller"
    exit 1
fi

log_info "dworm exec: SIGTERM is forwarded and reflected in the exit code..."
(cd "$DEVCONTAINER_PATH" && exec "$DWORM" exec -- sleep 4444 </dev/null) &
EXEC_PID=$!
wait_present "sleep 4444" || { log_fail "dworm exec did not start"; exit 1; }
kill -TERM "$EXEC_PID"
status=0
wait "$EXEC_PID" || status=$?
if [[ "$status" -ne 143 ]]; then
    log_fail "dworm exec exit status after SIGTERM = $status, want 143"
    exit 1
fi

log_info "Stopping dworm up removes the socket and kills running execs..."
python3 "$WORK_DIR/raw_client.py" "$SOCKET" sleep 4545 <"$WORK_DIR/stdin" >/dev/null 2>&1 &
CLIENT=$!
wait_present "sleep 4545" || { log_fail "raw exec did not start"; exit 1; }
kill -TERM "$DWORM_PID"
wait "$DWORM_PID" || true
DWORM_PID=
kill -KILL "$CLIENT" 2>/dev/null || true
wait "$CLIENT" 2>/dev/null || true
exec 3>&-
if [[ -e "$SOCKET" ]]; then
    log_fail "exec socket remains after dworm up exited"
    exit 1
fi
if ! wait_gone "sleep 4545" 8; then
    log_fail "exec process survived dworm up shutdown"
    exit 1
fi

log_pass "Exec over the bridge works and cleans up after its callers"
