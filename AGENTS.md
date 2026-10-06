# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository

## Project Overview

dworm is a Go CLI tool that provides devcontainer bridging. It consists of two cooperating binaries that communicate over a multiplexed stdin/stdout connection.

## Architecture

```
cmd/
├── dworm/main.go           # Host-side CLI entry point (Cobra)
└── dworm_endpoint/main.go  # Container-side binary entry point

test/e2e/                   # E2E test scripts (require Docker)
├── run-e2e.sh              # Main test runner
├── lib/common.sh           # Shared test utilities
├── lib/skip.sh             # Skip condition helpers
└── test-*.sh               # Individual test scripts

internal/
├── protocol/               # Shared between host and endpoint
│   ├── messages.go         # JSON message types (Init, PortUpdate), stream markers, ProtocolVersion
│   ├── exec.go             # Exec stream framing, ExecRequest/Reply/Exit, limits
│   ├── mux.go              # Yamux wrapper for multiplexing over stdin/stdout
│   ├── proxy.go            # BiProxy utility for bidirectional data copying
│   ├── constants.go        # Shared constants (socket paths, limits)
│   ├── writer.go           # CRWriter for terminal output, NewLogWriter (CR only on terminals)
│   └── testutil/harness.go # Test harness for in-process testing
├── host/                   # Host-side only
│   ├── container.go        # Devcontainer lifecycle (up/down/remove/rebuild via devcontainer CLI + docker), workspace folder lookup
│   ├── endpoint.go         # Injects endpoint binary, manages communication
│   ├── tunnel.go           # Port forwarding (listens locally, proxies to container)
│   ├── instance.go         # Per-workspace flock, runtime dir, state file (instance_unix.go: flock)
│   ├── execserver.go       # Local exec unix socket (raw/framed clients → bridge exec streams)
│   ├── execserver_linux.go # SO_PEERCRED check, POLLHUP disconnect detection for raw mode
│   ├── execclient.go       # `dworm exec` client for the exec socket (framed mode)
│   ├── status.go           # `dworm status` data (GetStatus, RunningInstance, WorkspaceFolder)
│   ├── shell.go            # `dworm shell`/`dworm exec` via docker exec (stdio passthrough, ExitError)
│   ├── agent.go            # SSH/GPG agent forwarding (accepts streams from endpoint)
│   └── tui/                # Terminal UI for interactive shell
│       ├── model.go        # Main TUI session with PTY and 3-goroutine architecture
│       ├── statusbar.go    # Status bar component (lipgloss-styled)
│       ├── messages.go     # Message types (PortMapping, PortUpdateMsg)
│       ├── styles.go       # lipgloss style definitions
│       └── fallback.go     # Non-TTY fallback mode
├── endpoint/               # Container-side only
│   ├── server.go           # Main server loop, handles control messages, dispatches host-opened streams
│   ├── exec.go             # Exec streams: spawns processes in own process groups, kills on disconnect
│   ├── portscanner.go      # Scans /proc/net/tcp for listening ports
│   ├── env.go              # Environment variable handling
│   ├── agent.go            # SSH agent forwarding (Unix socket listener)
│   ├── gpg.go              # GPG agent forwarding (Unix socket listener)
│   ├── gitconfig.go        # Git config file writing and credential helper setup
│   └── gitcred.go          # Git credential forwarding (Unix socket + helper client)
└── version/                # Version info and update checking
    ├── version.go          # Version variables (set via ldflags at build time)
    └── check.go            # GitHub API version check for update notifications
```

## Key Components

### Protocol (`internal/protocol/`)

- **Multiplexing**: Uses `github.com/hashicorp/yamux` over stdin/stdout
- **Control channel**: Stream 0, length-prefixed JSON messages (max 1MB via `MaxControlMessageSize`)
- **Other streams**: Stream 1+. Every stream starts with a 1-byte type marker. Host-opened:
  `StreamTypeTunnel` (0x10, TCP tunnel), `StreamTypeExec` (0x11, process execution).
  Endpoint-opened: `StreamTypeAgent`/`GPG`/`GitCred` (0x01-0x03)
- **Protocol version**: `protocol.ProtocolVersion` (currently 2) is sent in `InitMessage.ProtocolVersion`
  and echoed in `environment_ready` (`EnvironmentReadyMessage`). The endpoint rejects a mismatch with
  `init_error`; the host rejects a missing/mismatched echo. Host and endpoint always come from the same
  build (the endpoint is embedded and injected on every `up`); bump the version on any incompatible
  stream or message change.
- **BiProxy utility**: `protocol.BiProxy(conn1, conn2)` handles bidirectional copying with proper shutdown

Message types:
- `InitMessage`: env vars, agent forwarding config, GPG public keys, protocol version
- `environment_ready` / `init_error`: endpoint's answer to init
- `PortUpdateMessage`: contains `[]PortInfo` where each `PortInfo` has `Port int` and `Address string` (bind address)

Message flow:
1. Host sends `init` with env vars, agent forwarding config, and GPG public keys
2. Endpoint sends `port_update` when listening ports change (includes bind addresses)
3. Host opens new yamux stream for each tunnel connection
4. Stream header: `StreamTypeTunnel`, 4-byte port number; endpoint replies with 1-byte success response

Agent/credential forwarding (reverse direction, endpoint → host):
- Stream type markers: `StreamTypeAgent` (0x01) for SSH, `StreamTypeGPG` (0x02) for GPG, `StreamTypeGitCred` (0x03) for git credentials
- SSH: Endpoint creates socket at `protocol.SSHAgentSocketPath`, sets `SSH_AUTH_SOCK` env var
- GPG: Host exports public keys via `gpg --export --armor`, sends in `InitMessage.GPGPublicKeys`. Endpoint imports them via `gpg --import` before starting forwarder. Endpoint runs `gpgconf --list-dirs agent-socket` to find expected path (e.g., `~/.gnupg/S.gpg-agent`), kills existing agent, creates socket there
- Git: Endpoint creates socket at `protocol.GitCredentialSocket`, creates helper script at `protocol.GitCredentialHelperPath`, configures git to use it
- On client connect: endpoint opens yamux stream to host with type marker, host proxies to local agent socket (SSH/GPG) or runs `git credential` command (git)

Exec streams (host → endpoint, `exec.go`):
1. Host writes `StreamTypeExec`, then a length-prefixed (4-byte BE) JSON `ExecRequest`
   (`argv`, `cwd`, `env`, `id`; max `MaxExecHeaderSize` = 64KB)
2. Endpoint replies with a length-prefixed `ExecReply` (`ok`, `error`, `exit_code` = 127/126 when the
   command can't be started; also used for limits: `MaxConcurrentExecs` = 32)
3. Then frames `[1 byte type][4 byte BE length][payload]` (max 1MB): `0x00` stdin, `0x01` stdout,
   `0x02` stderr, `0x03` stdin EOF, `0x04` exit (`{"code":N,"signal":"TERM"}`, last frame; signals
   report code 128+n), `0x05` signal (`{"signal":"TERM"}`, sent to the process group)
4. A stream closed/reset before the exit frame is a kill request: SIGTERM to the process group,
   SIGKILL after `ExecKillGrace` (5s). The endpoint does the same for all execs when the bridge ends.

Shared constants (`internal/protocol/constants.go`):
- `SSHAgentSocketPath`, `GPGAgentSocketPath`, `GitCredentialSocket`, `GitCredentialHelperPath`
- `MaxControlMessageSize` (1MB), `MaxGitCredentialInput` (64KB)

### Version (`internal/version/`)

- `Version`, `Commit`, `Date` variables set at build time via ldflags
- `Info()` returns formatted version string for `--version` flag
- `CheckForUpdate()` queries GitHub API for latest release (async, 5s timeout)
- Skips update check for development builds (`Version == "dev"` or empty)
- Semver comparison in `isNewer()` handles `vX.Y.Z` format

### Host Binary (`cmd/dworm/`)

Entry point uses Cobra with subcommands: `up`, `down`, `shell`, `exec`, `status`, `remove`, `rebuild`, `self-update`

CLI flags:
- `--version` / `-v`: Show version info (handled by Cobra)
- Async update check runs at startup, prints warning after command completes if newer version exists

Key flows:
- `up`: AcquireInstanceLock → DevcontainerUp → InjectAndStart → SendInit → handle port updates → ForwardPort
- `down`: DevcontainerDown (finds container by label, docker stop)
- `shell` / `exec`: start in the workspace folder from `host.WorkspaceFolder`: the running `up`'s state
  file if it is for the same container, else `ResolveWorkspaceFolder` (`container.go`: the bind mount
  containing the workspace path, mapped into the container; "" = image default). `exec --workdir/-w`
  overrides it.
- `status [--json]`: `host.GetStatus` (see below). Exits 0 when there is no container or no `up`.
- `exec -- CMD...`: without a TTY and with a running `up`, runs over the exec socket (see "Exec over the
  bridge"). Otherwise runs `docker exec -i` through the `--with-env` launcher. Stdin is always attached;
  `-t` is added only when stdin and stdout are both terminals. stdout carries only the child's stdout
  (all diagnostics go to stderr). The child's exit status becomes dworm's exit status
  (`host.ExitError`, handled in `main()`); SIGINT/SIGTERM are forwarded to the docker CLI.
- `remove [--force]`: DevcontainerRemove (finds container by label, docker stop + rm + rmi; prompts for confirmation unless `--force`)
- `rebuild`: DevcontainerRebuild (calls `devcontainer up --remove-existing-container`; rebuilds and exits, user runs `up` separately)

### Exec over the bridge (`internal/host/execserver.go`, `internal/endpoint/exec.go`)

Every running `dworm up` (TUI and daemon) listens on `<runtime dir>/<hash>.sock` (mode 0600, removed on
exit, path in the state file as `exec_socket`). On Linux, `SO_PEERCRED` restricts it to the same UID.

Client protocol (one connection = one process):
- Client sends one JSON line: `{"version":1, "argv":[...], "cwd":"...", "env":{...}, "mode":"raw"|"framed"}`.
  Unknown fields are rejected; `cwd` defaults to the workspace folder; `env` is merged over the
  published dworm environment exactly like `--with-env` (inherited env → published snapshot → `env`);
  `mode` defaults to `raw`.
- Host replies with one JSON line: `{"ok":true,"id":"<12 hex>"}` or `{"ok":false,"error":"…"}` (then
  closes; `exit_code` 127/126 when the command could not be started).
- `raw`: afterwards a plain byte stream. Client bytes = stdin, client half-close (`shutdown(SHUT_WR)`) =
  stdin EOF, host→client bytes = stdout only. stderr goes to dworm's log as `[exec <id>] line`
  (line-buffered, ≤50 lines/s, then a suppression notice). After the exit frame and drained stdout,
  the host closes the connection; there is no exit code. A full disconnect (detected via POLLHUP on
  Linux, so it differs from a half-close) kills the process.
- `framed`: the bridge frames are passed through (client may send stdin/stdin-EOF/signal; receives
  stdout/stderr/exit). Socket EOF before the exit frame kills the process.
- Kill request = the host closes the bridge stream; the endpoint terminates the process group.

The endpoint spawns with `Setpgid`, as the endpoint's user, resolving `argv[0]` with the merged `PATH`.

`dworm exec` uses the socket in framed mode when stdin and stdout are not both terminals and
`RunningInstance` reports an `exec_socket`; otherwise (or on `ErrExecSocketUnavailable` before the
command started) it falls back to `docker exec -i[t]`. TTY sessions always use docker (`-t`). The
client forwards SIGINT/SIGTERM/SIGHUP as signal frames; the exit frame's code becomes dworm's exit code.

### `dworm up` instance lock, state file, and lifecycle (`internal/host/instance.go`)

- Runtime dir: `$XDG_RUNTIME_DIR/dworm/`, fallback `/tmp/dworm-$UID/` (created/chmodded to 0700).
- Files are named after `sha256(absolute workspace path)[:12]`: `<hash>.lock`, `<hash>.json`
  (and `<hash>.sock`, see exec over the bridge). The short name keeps socket paths < 108 bytes.
- `up` (TUI and daemon) takes `LOCK_EX|LOCK_NB` on `<hash>.lock` before anything else. If held:
  exit code **3** (`exitAlreadyRunning` in `cmd/dworm/main.go`). The lock file is never deleted.
  `InstanceRunning` probes with a momentary `LOCK_SH|LOCK_NB`; acquisition retries for ~250ms so a
  concurrent probe cannot make `up` fail.
- State file `<hash>.json`, rewritten atomically (temp file + rename) by `StateFile.Update`, removed
  on exit. Fields: `pid`, `workspace_path`, `container_id`, `container_name`, `remote_user`,
  `workspace_folder` (in-container path), `started_at`, `endpoint_connected`, `ports`
  (`[{port, address, local_port}]`: container port, host bind address, host port), `exec_socket`.
  Updated after container start, endpoint init, exec socket start, every forwarded-port change, and on
  bridge failure.
  Only trust it while the lock is held.
- Signals: `stopCtx` (SIGINT/SIGTERM) → clean shutdown, exit 0, container keeps running.
  Bridge failure (control stream EOF) → exit 1 so systemd `Restart=on-failure` restarts it. Because
  systemd signals the whole cgroup, a failure that coincides with a stop signal (500ms grace) is
  treated as a stop.
- Logs: `protocol.NewLogWriter` uses `CRWriter` only when the file is a terminal; the endpoint logs
  plain lines to its stderr pipe.

### `dworm status --json` (`internal/host/status.go`)

Fields (all always present unless noted):
- `workspace_path`: absolute host path of the workspace (the current directory)
- `container`: `null` when no container (running or stopped) has label
  `devcontainer.local_folder=<workspace_path>`, else an object:
  - `id` (12-char short ID), `name`, `running`
  - `remote_user`: from the running `up`'s state, else the `devcontainer.metadata` label (last
    `remoteUser`, then last `containerUser`, then the image user)
  - `workspace_folder`: from the running `up`'s state, else the bind-mount heuristic
- `up`: `running` (true only if the lock is held, checked with a non-blocking flock),
  `endpoint_connected`, `ports` (`[{port, address, local_port}]`, `[]` when not running), and when
  running `pid`, `started_at` (RFC 3339, omitted if unknown), `exec_socket` (omitted if disabled)
- `dworm_version`: `version.Version`

The human-readable output (default) shows the same information.

### Endpoint Binary (`cmd/dworm_endpoint/`)

Two modes of operation:

**Server mode** (default):
1. Accepts yamux session (server mode)
2. Waits for init message, sets env vars, writes git config, starts forwarders
3. Runs port scanner goroutine (2s interval)
4. Accepts tunnel streams, connects to local ports

**Credential helper mode** (`--credential-helper <action>`):
- Invoked by the git credential helper script
- Connects to `/tmp/dworm-git-credential.sock`
- Forwards credential request to host via the socket

### Port Scanner (`internal/endpoint/portscanner.go`)

Parses `/proc/net/tcp` and `/proc/net/tcp6`:
- Format: `sl local_address rem_address st ...`
- Local address is `IP:PORT` in hex (little-endian for IPv4, per-word little-endian for IPv6)
- State `0A` = TCP_LISTEN
- Filters to ports 1024-20000
- Returns `[]protocol.PortInfo` with both port number and bind address (e.g., `127.0.0.1`, `::1`, `0.0.0.0`)

Address parsing (`parseHexAddress`):
- IPv4: 8 hex chars, stored little-endian (e.g., `0100007F` → `127.0.0.1`)
- IPv6: 32 hex chars, stored as 4 little-endian 32-bit words

When connecting to a detected port, endpoint uses the actual bind address:
- `0.0.0.0` → connects to `127.0.0.1`
- `::` → connects to `[::1]`
- `::1` → connects to `[::1]`
- Other addresses → connects directly to that address

If same port bound to multiple addresses, `preferAddress()` selects the most connectable:
priority: `0.0.0.0`/`::` > specific IPs > `127.0.0.1`/`::1`

### Tunnel Manager (`internal/host/tunnel.go`)

For each detected port:
1. Listen on configured bind address (default `127.0.0.1`, configurable via `--bind` flag)
2. On connection: open yamux stream, send port header, proxy bidirectionally

### TUI Shell Session (`internal/host/tui/`)

Interactive shell with status line showing container name and forwarded ports:
- Uses PTY for proper terminal handling (resize, raw mode)
- 3-goroutine architecture: output reader, input handler, event handler
- Status bar at bottom styled with lipgloss (reverse video)
- Scroll region restricts shell output to above status bar
- Toggle expanded port view with Ctrl+G
- Clear screen detection re-renders status bar after vim/htop/etc
- Falls back to basic `docker exec` shell if not running in a TTY

Key files:
- `model.go`: Main `Run()` function and session orchestration
- `statusbar.go`: `StatusBar` component with collapsed/expanded modes
- `messages.go`: `PortMapping` and `PortUpdateMsg` types
- `fallback.go`: Non-TTY fallback for piped/scripted usage

## Build

```bash
make build          # Both binaries
make build-host     # Host only (native)
make build-endpoint # Endpoint only (linux/amd64)
make fmt            # Format code
make tidy           # Update go.mod
```

Endpoint is always built for `GOOS=linux GOARCH=amd64` since it runs in containers.

**Version injection**: The Makefile automatically injects version info via ldflags:
- `Version`: from `git describe --tags --always --dirty` (e.g., `v1.0.0`, `v1.0.0-5-gabc1234-dirty`, or `dev`)
- `Commit`: short git commit hash
- `Date`: build timestamp (UTC ISO 8601)

The GitHub Actions release workflow also injects version from the git tag.

## Testing

### Unit Tests

```bash
make test-unit     # Run all unit tests (no Docker required)
make test-race     # Run with race detector
make test-cover    # Generate coverage report (coverage.html)
```

Test files:
- `internal/protocol/mux_test.go` - Yamux multiplexing, control messages, streams
- `internal/protocol/messages_test.go` - Message encoding/decoding
- `internal/endpoint/portscanner_test.go` - /proc/net/tcp parsing, port diff logic
- `internal/host/agent_test.go` - SSH/GPG/git credential stream routing
- `internal/host/instance_test.go` - Instance lock exclusivity/probing, state file
- `internal/host/status_test.go` - Status JSON shape, metadata/remote user parsing (fake `docker`)
- `internal/protocol/exec_test.go` - Exec frame/message encoding and size limits
- `internal/endpoint/exec_test.go` - Exec streams over the harness: stdio, env/cwd, signals, exit codes,
  rejections/limits, concurrency, process-group kill on disconnect and shutdown
- `internal/host/execserver_test.go` - Exec socket against a fake endpoint: raw ↔ framed translation,
  half-close vs disconnect, validation, CLI client, stderr rate limiting
- `internal/host/shell_test.go` - `docker exec` argument building, stdio/exit code passthrough (fake `docker` on PATH)

**Test harness** (`internal/protocol/testutil/harness.go`):
- Connects host and endpoint muxes over `io.Pipe()` for in-process testing
- No Docker required - uses real yamux over memory pipes
- Usage: `h, _ := testutil.NewTestHarness(); defer h.Close()`
- Access muxes via `h.HostMux` and `h.EndpointMux`

### E2E Tests

```bash
make test-e2e      # Run E2E tests (requires Docker, skips gracefully if unavailable)
make test          # Run all tests (unit + e2e)
```

E2E scripts in `test/e2e/`:
- `run-e2e.sh` - Main runner with Docker auto-detection
- `test-port-forward.sh` - Port forwarding test
- `test-env-vars.sh` - Environment variable forwarding
- `test-exec-stdio.sh` - `dworm exec` stdin/stdout passthrough, stdin EOF, exit codes, working directory
- `test-exec-socket.sh` - Raw socket client (python3), `dworm exec` via the socket, no processes left
  after killed callers or `up` shutdown (`pgrep` in the container)
- `test-daemon.sh` - `up --daemon` single instance (exit 3), state file, `status --json`, SIGTERM exit 0, bridge failure exit ≠ 0
- `test-ssh-agent.sh` - SSH agent forwarding (conditional - skips if no agent)
- `test-gpg-agent.sh` - GPG agent forwarding (conditional)
- `test-git-creds.sh` - Git credential forwarding (conditional)
- `test-rebuild.sh` - Rebuild command
- `test-remove.sh` - Remove command (container + image cleanup)

Conditional tests use exit code 77 to skip (autotools convention).

### Manual Testing

```bash
# Start test devcontainer (run from project root)
dworm up --daemon

# In another terminal, start a server in container
docker exec <container> python3 -m http.server 8080

# Test tunnel
curl http://localhost:8080
```

## Common Modifications

### Add new message type

1. Add const in `internal/protocol/messages.go`
2. Add struct type
3. Add decode function
4. Handle in `internal/endpoint/server.go` (endpoint receives)
5. Or handle in `cmd/dworm/main.go` control message loop (host receives)

### Add new host-opened stream type

1. Add a `StreamType…` marker (0x1x range) in `internal/protocol/messages.go`
2. Write it as the first byte when opening the stream on the host
3. Dispatch it in `Server.handleStream` (`internal/endpoint/server.go`)
4. Bump `protocol.ProtocolVersion`

### Add new CLI flag

1. Add to `cmd/dworm/main.go` in relevant command setup
2. Pass through to appropriate internal function

### Change port range

Edit constants in `internal/endpoint/portscanner.go`:
```go
const (
    MinPort = 1024
    MaxPort = 20000
)
```

### Add new credential forwarding

SSH, GPG, and git credential forwarding are implemented. To add other credential forwarding:
1. Add stream type constant in `internal/protocol/messages.go` (e.g., `StreamTypeMyAgent byte = 0x04`)
2. Add socket path constant in `internal/protocol/constants.go`
3. Add fields to `InitMessage` for forwarding config
4. Create Unix socket listener in endpoint (similar to `gpg.go` for socket-based, or `gitcred.go` for command-based)
5. Use `protocol.BiProxy(conn1, conn2)` for bidirectional data copying
6. Extend `host/agent.go` `handleStream()` switch to handle new stream type
7. Update `cmd/dworm/main.go` to detect host config, send config in init, set env vars if needed

**Git credential forwarding architecture** (for reference):
```
Container: git → helper script → endpoint binary (--credential-helper) → socket → forwarder → yamux → host
Host: yamux stream → git credential fill/approve/reject → response
```

## Dependencies

- `github.com/spf13/cobra` - CLI framework
- `github.com/hashicorp/yamux` - Stream multiplexing
- `github.com/charmbracelet/lipgloss` - Terminal styling for status bar
- `github.com/creack/pty` - PTY handling for shell sessions
- `golang.org/x/term` - Terminal mode handling

## Error Handling Patterns

- Host logs to stderr with `[host]` prefix
- Endpoint logs to stderr with `[endpoint]` prefix (visible on host via docker exec stderr)
- Tunnel manager logs with `[tunnel]` prefix
- Control channel EOF is normal on shutdown
