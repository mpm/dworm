# dworm — Development Wormhole

A CLI tool that bridges your host machine and devcontainer environments, providing automatic port forwarding and environment injection without requiring VS Code.

## Features

- **Automatic Port Forwarding**: Detects listening ports inside the container (1024-20000) and forwards them to localhost
- **SSH Agent Forwarding**: Use your host SSH keys inside the container (automatic when `SSH_AUTH_SOCK` is set)
- **GPG Agent Forwarding**: Sign commits with your host GPG keys (automatic when gpg-agent is running)
- **Git Configuration Forwarding**: Host's `~/.gitconfig` is copied to the container (user.name, email, aliases, etc.)
- **Git Credential Forwarding**: Push/pull to private repos using host's credential helpers
- **Environment Variable Injection**: Pass environment variables from host to container
- **Project Environment Configuration**: Load `.dworm.env` and run host commands to generate and periodically refresh variables across dworm shells
- **Shell Access**: Interactive shell with all forwarding active, on a PTY in the container
- **One-shot Command Execution**: Run a command in the container and exit
- **One Background Instance per Workspace**: Shared by all commands; it reconnects by itself when
  the container restarts
- **Integration with Other Tools**: Unix socket for running processes, `status --json`, event stream

## Requirements

- Go 1.24.11+ (for building from source)
- Docker
- [devcontainer CLI](https://github.com/devcontainers/cli) (`npm install -g @devcontainers/cli`)

## Installation

### Quick install

```bash
curl -fsSL https://raw.githubusercontent.com/mpm/dworm/main/install.sh | sh
```

This installs to `~/.local/bin`. To install elsewhere:

```bash
curl -fsSL https://raw.githubusercontent.com/mpm/dworm/main/install.sh | DWORM_INSTALL_DIR=/usr/local/bin sh
```

To install a specific version:

```bash
curl -fsSL https://raw.githubusercontent.com/mpm/dworm/main/install.sh | sh -s v0.1.0
```

### Build from source

```bash
git clone https://github.com/mpm/dworm.git
cd dworm
make build

# Binaries are in ./bin/
# - bin/dworm          (host CLI with both Linux endpoints embedded)
# - bin/dworm_endpoint (standalone amd64 endpoint for development)
```

Install only `bin/dworm`. Hosts are distributed for Linux and macOS, amd64 and
arm64; every host includes Linux amd64 and arm64 endpoints. The container image's
platform determines which endpoint is injected, including emulated containers.
Old separately installed `dworm_endpoint` files are ignored and no longer needed.

Use `make prepare-endpoints` before direct `go build` or `go test ./...` commands.
`make build-host`, `make test-unit`, and `make test-race` prepare assets automatically.
`make clean` removes them. Use this checkout-based workflow rather than
`go install ...@latest`, which cannot generate the required embedded executables.

### Updating

```bash
dworm self-update
```

This checks for the latest stable release, verifies its SHA-256 checksum, and
replaces the running executable's resolved path (preserving symlinks). It works
outside a workspace without Docker. The next invocation uses the new host and
bundled endpoints; active sessions are not restarted. Already-current or newer
versions are left alone. Development/dirty builds cannot self-update. Destination
directory permissions must allow replacement; errors include the release URL for
manual installation.

Versions before v0.6.0 need the installer or a manual installation once to gain
the `self-update` command. The installer still supports older two-binary archives.

## Usage

### Start a devcontainer with port forwarding

```bash
# Start container and bridge, then open a shell (run from a directory
# containing .devcontainer/devcontainer.json)
dworm up

# Only make sure everything runs, then return
dworm up -d

# With environment variables
dworm up --env API_KEY=secret --env DEBUG=true

# Use a different devcontainer.json
dworm up --config /path/to/devcontainer.json
dworm up -c .devcontainer/custom.json
```

### The workspace instance

One long-lived background **instance** per workspace owns the bridge to the
container: port forwarding, agent and credential forwarding, the project
environment, and a unix socket for running processes. Every dworm command is a
client: it makes sure the instance runs (starting it in the background if
needed, which runs `devcontainer up` and so also starts a stopped container) and
then talks to it. Clients come and go; the instance stays until it is stopped.

- `dworm up` ensures the instance and, on a terminal, opens the shell. Leaving
  the shell leaves the instance (and your forwarded ports) running. `dworm up -d`,
  or `dworm up` without a terminal, waits until the instance is ready, prints a
  one-line summary, and exits 0, also when it was already running.
- `dworm shell` and `dworm exec` ensure it too (`--no-start` makes them fail
  instead of starting a stopped container).
- `dworm stop` stops the instance and leaves the container running;
  `dworm down` stops both. `dworm remove` and `dworm rebuild` stop it first.
- While the instance starts, clients show the `devcontainer up` output on a
  terminal (or with `-v`); `--timeout` (default 15m) bounds the wait.
- `-e`/`-c`/`--bind` on `dworm up`/`dworm shell` only apply when they start the
  instance. If it already runs with other `-e` values you get a warning; run
  `dworm stop` first. `dworm exec -e` applies to that command only.
- Clients only talk to an instance of the same dworm version; after an upgrade,
  run `dworm stop` once.

If the bridge breaks (the endpoint dies, the container restarts), the instance
reconnects by itself: it re-injects the endpoint and re-sends the environment.
While the container is down it retries for two minutes (enough for
`docker restart`), then stops. During a reconnect, forwarded ports refuse new
connections and running processes are lost (`dworm exec` exits with 255 and
prints `dworm: bridge lost`); the shell's status bar shows `reconnecting...` and
a new shell starts once the instance is ready again.

The instance's files live in `$XDG_RUNTIME_DIR/dworm/` (or `/tmp/dworm-$UID/`,
mode `0700`), named after a hash of the workspace path: an `flock` lock, a state
file, the socket, and the log of a background instance (5 MiB, rotated once).
`dworm logs` shows recent log lines, `dworm logs -f` follows them.

### Running under systemd

The instance can also run in the foreground with `dworm up --foreground`
(`--daemon` is a deprecated alias), e.g. as a `Type=simple` systemd unit:

- it exits immediately with **exit code 3** if an instance already runs;
- SIGTERM/SIGINT (or `dworm stop`) shuts it down cleanly and exits 0, leaving
  the container running (`dworm down` stops it);
- it reconnects by itself and exits non-zero only on unrecoverable errors (e.g.
  `devcontainer up` failed), so `Restart=on-failure` stays meaningful;
- when stderr is not a terminal, logs use plain `\n` line endings, one line per
  event.

```ini
# ~/.config/systemd/user/dworm@.service
[Unit]
Description=dworm bridge for %i

[Service]
Type=simple
WorkingDirectory=%h/projects/%i
ExecStart=%h/.local/bin/dworm up --foreground
Restart=on-failure
RestartSec=5
# Optional: don't retry while another instance holds the workspace
RestartPreventExitStatus=3

[Install]
WantedBy=default.target
```

A unit is optional: `dworm up -d` starts the instance in the background on
demand, and every other command does the same.

### Status

`dworm status --json` combines the container's state with the instance's live
state:

```json
{
  "workspace_path": "/home/me/projects/app",
  "container": {"id": "2e7d99a944c1", "name": "app-1", "running": true,
                "remote_user": "vscode", "workspace_folder": "/workspaces/app"},
  "up": {"running": true, "pid": 751026, "started_at": "2026-10-06T13:15:58Z",
         "state": "ready", "mode": "detached", "endpoint_connected": true,
         "reconnects": 0, "clients": 1,
         "ports": [{"port": 3000, "address": "127.0.0.1", "local_port": 3000}],
         "exec_socket": "/run/user/1000/dworm/5497057b0012.sock",
         "log_path": "/run/user/1000/dworm/5497057b0012.log",
         "dworm_version": "v0.8.0"},
  "dworm_version": "v0.8.0"
}
```

`container` is `null` when the workspace has no container, and `up.running` is
true only while an instance holds the workspace lock. `up.state` is `starting`,
`connecting`, `ready`, `reconnecting`, `stopping`, `stopped`, or `failed`.
`clients` counts attached exec sessions and event subscriptions. Both "no
container" and "no instance" exit 0; non-zero exit codes mean dworm could not
determine the status (e.g. Docker is unavailable).

### The instance socket for other programs

The instance listens on a unix socket (`exec_socket` in `dworm status --json`,
mode `0600`, same user only). Each connection starts with one JSON request line
and gets one JSON reply line; `op` selects what it does (without `op` it is an
`exec`, as in v0.7.0):

```text
→ {"version":1, "argv":["opencode","acp"], "cwd":"/workspaces/app", "env":{"FOO":"bar"}, "mode":"raw"}
← {"ok":true,"id":"3f9a1c2b7d10"}            or  {"ok":false,"error":"…"}
→ {"version":1, "op":"events", "history":20}  (then newline-delimited JSON events)
→ {"version":1, "op":"status"}                ← {"ok":true,"status":{…}}
→ {"version":1, "op":"stop"}
```

An exec runs one process in the container with the published dworm environment.
`cwd` defaults to the workspace folder and `env` overrides the dworm environment.
In `raw` mode the connection is then a plain byte stream: your bytes are the
process's stdin, half-closing the connection (`shutdown(SHUT_WR)`) is stdin EOF,
and you receive only its stdout. stderr goes to dworm's log as `[exec <id>]`
lines. When the process exits and its stdout is drained, dworm closes the
connection (raw mode has no exit code). Closing the connection before the
process exits terminates its process group. `framed` mode (used by `dworm exec`)
carries stdin, stdout, stderr, signals, window sizes, and the exit status as
frames, and can run the process on a PTY (`"tty":{"rows":24,"cols":80}`); see
`AGENTS.md`. Up to 32 processes can run concurrently per container. Before the
instance is ready, exec requests get `{"ok":false,"error":"not ready","code":"not_ready"}`.

The event stream starts with a snapshot (current state, ports, recent log lines)
and continues with live events:

```json
{"t":"2026-10-07T10:00:00Z","type":"state","state":"reconnecting","reason":"bridge_lost"}
{"t":"…","type":"ports","ports":[{"port":3000,"address":"127.0.0.1","local_port":3000}]}
{"t":"…","type":"log","source":"endpoint","level":"info","message":"…"}
{"t":"…","type":"exec_started","id":"…","argv":["opencode","acp"],"tty":false}
{"t":"…","type":"exec_exited","id":"…","code":0,"signal":""}
{"t":"…","type":"client","attached":2}
```

A subscriber that reads too slowly misses events and gets
`{"type":"dropped","count":N}` instead; it never slows the instance down.
`dworm logs -f --json` prints this stream.

### Project environment and host startup commands

Place optional `.dworm.config` and `.dworm.env` files in the workspace directory
from which you run `dworm up`.

`.dworm.config` uses TOML:

```toml
bind = "127.0.0.1"

[host_env]
command = ["bash", "./scripts/github-token.sh"]
refresh_interval = "1h"
timeout = "30s"
```

The command runs **on the host**, in the workspace directory, with the host's
environment. Its first successful execution finishes before the container is
started. Arguments are passed directly; use `["bash", "-c", "..."]` if you need
shell syntax. The timeout defaults to 30 seconds. Omit `refresh_interval` to run
only once; durations must be positive Go durations such as `30s`, `5m`, or `1h`.
Explicit `dworm up --bind ...` takes precedence over the configured bind address.

The command's stdout must contain only dotenv assignments. Send diagnostics to
stderr. For example, `scripts/github-token.sh` could contain:

```bash
#!/usr/bin/env bash
set -euo pipefail
token="$(your-token-generator)"
printf 'GH_TOKEN=%s\n' "$token"
```

`.dworm.env` supplies static values:

```dotenv
APP_ENV=development
GH_HOST=github.com
MY_SETTING="a value with spaces"
```

Both sources support `KEY=VALUE`, optional `export`, blank lines, comments, single
or double quotes, and empty values. Double-quoted values support escapes such as
`\n`. Assignments are line-oriented; use escaped newlines for multiline values.
There is no variable interpolation or command substitution: `$HOME` and `$(...)`
are literal data. Variable names must be shell identifiers; `_DWORM_` names are
reserved for internal use. Command output is limited to 512 KiB.

Precedence (highest first):

1. CLI `--env` / `-e` overrides
2. Host command output
3. `.dworm.env`
4. The existing container environment

`dworm up` publishes a shared, private environment snapshot inside the container.
Its shell, separate `dworm shell` sessions, and `dworm exec` all use this snapshot.
An override passed to `dworm exec -e KEY=value` applies to that command only.
Interactive Bash shells reload managed variables at each prompt, preserving the
user's `.bashrc` and existing prompt hooks. Already-running programs and shell
scripts retain their inherited environment; they must reload credentials themselves
or be restarted. Direct `docker exec` invocations and container startup services
do not automatically load dworm's environment.

Refreshes run serially while the workspace instance is running, in the
background or in the foreground (`--foreground`). Each successful
run replaces the previous command output, including removing omitted variables
and falling back to lower-priority values. Initial command failure or invalid
output aborts startup; later failures retain the last successful values and retry
at the next interval. Shells and commands never start another refresh loop.
Configuration and `.dworm.env` are read once when the instance starts; run
`dworm stop` to pick up changes.

Snapshots are atomically written to `/tmp/dworm/environment.json` with mode `0600`
in a `0700` directory. Generated values are not written to the workspace or logged
by dworm; command stderr is forwarded as diagnostics. If the container remains
running after the instance stops, shells can use the last snapshot, but refreshes stop.

When upgrading, install `dworm`, which includes its matching endpoints, and run
`dworm stop` so the next command starts an instance that injects the new
endpoint. Reopen existing shells to activate the environment reload hook.

### Other commands

```bash
# Stop the instance, keep the container running
dworm stop

# Stop the instance and the container
dworm down

# Open a shell (TUI with status bar; Ctrl+G shows ports and logs)
dworm shell

# Run a command in the container and exit
dworm exec -- npm test
dworm exec -w /tmp -- ls          # explicit working directory
printf 'data' | dworm exec -- cat # stdin/stdout are passed through byte for byte
dworm exec --no-bridge -- ls      # plain docker exec, bypassing the instance

# Show the instance's log and events
dworm logs
dworm logs -f --json

# Show container, instance, and forwarded port status
dworm status
dworm status --json   # machine-readable

# Remove container and its image (prompts for confirmation)
dworm remove
dworm remove --force  # skip confirmation

# Rebuild the container from scratch
dworm rebuild
```

`dworm exec` runs the command over the instance, so the endpoint owns the
process: if the caller disappears (even via SIGKILL), its process group gets
SIGTERM and, after 5 seconds, SIGKILL. On a terminal (`dworm exec -- bash`), the
command runs on a PTY in the container and a disconnect hangs up the session
(SIGHUP first). Only `--no-bridge` uses `docker exec`, which cannot clean up after
a killed client.

`dworm shell` and `dworm exec` start in the container's workspace folder (the
in-container path of the project directory), unless `dworm exec --workdir/-w`
selects another directory. `dworm exec` always forwards stdin, uses a PTY only
when stdin and stdout are both terminals, writes only the command's output to
stdout, and exits with the command's exit status.

### Example workflow

```bash
# Terminal 1: Start dworm from your project directory and work in the shell
$ dworm up
developer@container:~/workspace$ npm run dev

# Terminal 2: Access your app
$ curl http://localhost:3000
Hello from container!

# Terminal 3: Another shell, or a one-off command; the forwarded ports stay
# available when you leave either shell
$ dworm exec -- npm test

# Done for the day
$ dworm down
```

### SSH agent forwarding

SSH agent forwarding is automatic when `SSH_AUTH_SOCK` is set on your host:

```bash
# Verify SSH agent is running on host
$ ssh-add -l
256 SHA256:... user@host (ED25519)

# Start dworm (agent forwarding is automatic)
$ dworm up

# Inside the container, your keys are available
developer@container:~$ ssh-add -l
256 SHA256:... user@host (ED25519)

developer@container:~$ git clone git@github.com:user/repo.git
# Works without copying keys!
```

### GPG agent forwarding

GPG agent forwarding is automatic when gpg-agent is running on your host:

```bash
# Verify GPG agent is running and has keys
$ gpg --list-secret-keys
/home/user/.gnupg/pubring.kbx
-----------------------------
sec   ed25519 2024-01-01 [SC]
      ABC123...

# Start dworm (GPG forwarding is automatic)
$ dworm up

# Inside the container, sign commits with your host key
developer@container:~$ git commit -S -m "Signed commit"
```

### Git configuration and credentials

Your host's `~/.gitconfig` is automatically copied to the container, so `user.name`, `user.email`, aliases, and other settings work seamlessly.

Git credential forwarding proxies credential requests to the host, allowing you to push/pull from private repositories:

```bash
$ dworm up

# Inside the container
developer@container:~$ git push origin main
# Uses host's credential helper - no authentication prompts!
```

## How It Works

1. **dworm** (host) loads project environment configuration, runs the configured host command, and then starts the devcontainer using the devcontainer CLI
2. It injects **dworm_endpoint** binary into the container
3. Host and endpoint communicate over stdin/stdout of `docker exec`; the same
   bridge carries port tunnels, agent/credential forwarding, endpoint logs, and
   the processes of `dworm shell` and `dworm exec`. A background instance per
   workspace owns the bridge and reconnects it when it breaks
4. Endpoint scans `/proc/net/tcp` for listening ports and reports changes
5. Host binds matching ports locally and tunnels traffic through the multiplexed connection
6. The endpoint publishes a shared environment snapshot for dworm shells and commands; scheduled host-command runs replace that snapshot

```
┌─────────────────┐                    ┌─────────────────┐
│      Host       │                    │    Container    │
│                 │                    │                 │
│  dworm          │◄──── yamux ───────►│  dworm_endpoint │
│    │            │   (stdin/stdout)   │       │         │
│    ▼            │                    │       ▼         │
│  localhost:8080 │◄─── tunnel ───────►│  127.0.0.1:8080 │
│                 │                    │   (your app)    │
└─────────────────┘                    └─────────────────┘
```

## Configuration

dworm uses standard devcontainer configuration. Create a `.devcontainer/devcontainer.json` in your project:

```json
{
  "name": "my-project",
  "image": "mcr.microsoft.com/devcontainers/base:debian",
  "workspaceFolder": "/workspace",
  "workspaceMount": "source=${localWorkspaceFolder},target=/workspace,type=bind"
}
```

Optional [`.dworm.config` and `.dworm.env`](#project-environment-and-host-startup-commands)
configure host commands, environment variables, and port binding. They complement
`devcontainer.json`; the CLI `--config` flag still selects a devcontainer configuration.

## Testing

### Unit tests

Unit tests require no Docker. Protocol tests connect the host and endpoint over
`io.Pipe()`. Environment tests also use temporary files and local shell processes
to verify command execution and independent Bash-session refreshes.

```bash
make test-unit     # Run all unit tests
make test-race     # Run with Go's race detector
make test-cover    # Generate HTML coverage report
```

### End-to-end tests

E2E tests use a real devcontainer to verify port forwarding, environment refresh,
agent forwarding, and credential forwarding. They require Docker and will skip
gracefully if Docker is unavailable.

```bash
make test-e2e      # Run E2E tests
make test          # Run both unit and E2E tests
```

After `make build`, run `./test/e2e/test-project-env.sh` for the isolated project
environment test. It checks host execution before container creation, precedence,
refresh, removed variables, failed-refresh retention, and the last snapshot after
the host bridge exits.

Some E2E tests are conditional:
- **SSH agent test**: Skips if `SSH_AUTH_SOCK` is not set
- **GPG agent test**: Skips if gpg-agent is not running or has no keys
- **Git credential test**: Skips if no credential helper is configured

## Limitations

- Processes running in the container do not survive a bridge reconnect, and
  there is no way to reattach to a running process
- Port range limited to 1024-20000
- Linux containers only (amd64 and arm64 endpoints are embedded)

## License

MIT

## Releases

See the [v0.8.0 release notes](docs/releases/v0.8.0.md), earlier notes in
[docs/releases](docs/releases/), and the [maintainer release guide](docs/RELEASING.md).
