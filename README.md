# dworm — Development Wormhole

A CLI tool that bridges your host machine and devcontainer environments, providing automatic port forwarding and environment injection without requiring VS Code.

## Features

- **Automatic Port Forwarding**: Detects listening ports inside the container (1024-20000) and forwards them to localhost
- **SSH Agent Forwarding**: Use your host SSH keys inside the container (automatic when `SSH_AUTH_SOCK` is set)
- **GPG Agent Forwarding**: Sign commits with your host GPG keys (automatic when gpg-agent is running)
- **Git Configuration Forwarding**: Host's `~/.gitconfig` is copied to the container (user.name, email, aliases, etc.)
- **Git Credential Forwarding**: Push/pull to private repos using host's credential helpers
- **Environment Variable Injection**: Pass environment variables from host to container
- **Shell Access**: Interactive shell with all forwarding active
- **Daemon Mode**: Run in foreground for integration with other tools

## Requirements

- Go 1.21+
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

### Other commands

```bash
# Stop the container
dworm down

# Open a shell in a running container
dworm shell

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

1. **dworm** (host) starts the devcontainer using the devcontainer CLI
2. It injects **dworm_endpoint** binary into the container
3. Host and endpoint communicate over stdin/stdout of `docker exec`
4. Endpoint scans `/proc/net/tcp` for listening ports and reports changes
5. Host binds matching ports locally and tunnels traffic through the multiplexed connection

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

## Testing

### Unit tests

Unit tests run entirely in-memory using a test harness that connects the host and endpoint code over `io.Pipe()`. No Docker required.

```bash
make test-unit     # Run all unit tests
make test-race     # Run with Go's race detector
make test-cover    # Generate HTML coverage report
```

### End-to-end tests

E2E tests use a real devcontainer to verify port forwarding, agent forwarding, and credential forwarding work correctly. They require Docker and will skip gracefully if Docker is unavailable.

```bash
make test-e2e      # Run E2E tests
make test          # Run both unit and E2E tests
```

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
