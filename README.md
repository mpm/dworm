# dworm — Development Wormhole

A CLI tool that bridges your host machine and devcontainer environments, providing automatic port forwarding and environment injection without requiring VS Code.

## Features

- **Automatic Port Forwarding**: Detects listening ports inside the container (1024-20000) and forwards them to localhost
- **Environment Variable Injection**: Pass environment variables from host to container
- **Shell Access**: Interactive shell with all forwarding active
- **Daemon Mode**: Run in foreground for integration with other tools

## Requirements

- Go 1.21+
- Docker
- [devcontainer CLI](https://github.com/devcontainers/cli) (`npm install -g @devcontainers/cli`)

## Installation

```bash
# Clone and build
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
dworm up .

# Start in daemon mode (no shell, stays in foreground)
dworm up . --daemon

# With environment variables
dworm up . --env API_KEY=secret --env DEBUG=true
```

### Other commands

```bash
# Stop the container
dworm down .

# Open a shell in a running container
dworm shell .

# Check container status
dworm status .
```

### Example workflow

```bash
# Terminal 1: Start dworm
$ dworm up . --daemon
2024/01/09 22:39:03 Container started: myproject-dev
Forwarding localhost:3000 -> container:3000
Forwarding localhost:5432 -> container:5432

# Terminal 2: Access your app
$ curl http://localhost:3000
Hello from container!

# Terminal 3: Work in the container
$ dworm shell .
developer@container:~$ npm run dev
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

## Limitations (v1)

- No credential forwarding (SSH agent, GPG) yet
- No automatic reconnection on disconnect
- Port range limited to 1024-20000
- Linux containers only (endpoint binary is Linux amd64)

## License

MIT
