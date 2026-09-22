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
- **Shell Access**: Interactive shell with all forwarding active
- **One-shot Command Execution**: Run a command in the container and exit
- **Daemon Mode**: Run in foreground for integration with other tools

## Requirements

- Go 1.24.4+ (for building from source)
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
# - bin/dworm          (host CLI)
# - bin/dworm_endpoint (injected into container)
```

## Usage

### Start a devcontainer with port forwarding

```bash
# Start container and open shell (default)
# Run from a directory containing .devcontainer/devcontainer.json
dworm up

# Start in daemon mode (no shell, stays in foreground)
dworm up --daemon

# With environment variables
dworm up --env API_KEY=secret --env DEBUG=true

# Use a different devcontainer.json
dworm up --config /path/to/devcontainer.json
dworm up -c .devcontainer/custom.json
```

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
An override passed to `dworm shell -e KEY=value` stays pinned in that session.
Interactive Bash shells reload managed variables at each prompt, preserving the
user's `.bashrc` and existing prompt hooks. Already-running programs and shell
scripts retain their inherited environment; they must reload credentials themselves
or be restarted. Direct `docker exec` invocations and container startup services
do not automatically load dworm's environment.

Refreshes run serially while the original host-side `dworm up` process is alive,
including in `--daemon` mode (which remains in the foreground). Each successful
run replaces the previous command output, including removing omitted variables
and falling back to lower-priority values. Initial command failure or invalid
output aborts startup; later failures retain the last successful values and retry
at the next interval. A separate `dworm shell` never starts another refresh loop.
Configuration and `.dworm.env` are read once per `dworm up` invocation.

Snapshots are atomically written to `/tmp/dworm/environment.json` with mode `0600`
in a `0700` directory. Generated values are not written to the workspace or logged
by dworm; command stderr is forwarded as diagnostics. If the container remains
running after dworm exits, shells can use the last snapshot, but refreshes stop.

When upgrading, install both `dworm` and `dworm_endpoint` from the same release,
stop the old host-side dworm process, and run `dworm up` again to inject the new
endpoint. Reopen existing shells to activate the environment reload hook.

### Other commands

```bash
# Stop the container
dworm down

# Open a shell in a running container
dworm shell

# Run a command in a running container and exit
dworm exec -- npm test

# Check container status
dworm status

# Remove container and its image (prompts for confirmation)
dworm remove
dworm remove --force  # skip confirmation

# Rebuild the container from scratch
dworm rebuild
```

### Example workflow

```bash
# Terminal 1: Start dworm from your project directory
$ dworm up --daemon
2024/01/09 22:39:03 Container started: myproject-dev
2024/01/09 22:39:03 SSH agent forwarding enabled
Forwarding localhost:3000 -> container:3000
Forwarding localhost:5432 -> container:5432

# Terminal 2: Access your app
$ curl http://localhost:3000
Hello from container!

# Terminal 3: Work in the container
$ dworm shell
developer@container:~$ npm run dev
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
3. Host and endpoint communicate over stdin/stdout of `docker exec`
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

- No automatic reconnection on disconnect
- Port range limited to 1024-20000
- Linux containers only (endpoint binary is Linux amd64)

## License

MIT

## Releases

See the [v0.5.0 release notes](docs/releases/v0.5.0.md) and the
[maintainer release guide](docs/RELEASING.md).
