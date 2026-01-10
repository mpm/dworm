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
│   ├── messages.go         # JSON message types (Init, PortUpdate)
│   ├── mux.go              # Yamux wrapper for multiplexing over stdin/stdout
│   ├── proxy.go            # BiProxy utility for bidirectional data copying
│   ├── constants.go        # Shared constants (socket paths, limits)
│   ├── writer.go           # CRWriter for terminal output
│   └── testutil/harness.go # Test harness for in-process testing
├── host/                   # Host-side only
│   ├── container.go        # Devcontainer lifecycle (up/down via devcontainer CLI + docker)
│   ├── endpoint.go         # Injects endpoint binary, manages communication
│   ├── tunnel.go           # Port forwarding (listens locally, proxies to container)
│   ├── shell.go            # Interactive shell via docker exec
│   └── agent.go            # SSH/GPG agent forwarding (accepts streams from endpoint)
└── endpoint/               # Container-side only
    ├── server.go           # Main server loop, handles control messages
    ├── portscanner.go      # Scans /proc/net/tcp for listening ports
    ├── env.go              # Environment variable handling
    ├── agent.go            # SSH agent forwarding (Unix socket listener)
    ├── gpg.go              # GPG agent forwarding (Unix socket listener)
    ├── gitconfig.go        # Git config file writing and credential helper setup
    └── gitcred.go          # Git credential forwarding (Unix socket + helper client)
```

## Key Components

### Protocol (`internal/protocol/`)

- **Multiplexing**: Uses `github.com/hashicorp/yamux` over stdin/stdout
- **Control channel**: Stream 0, length-prefixed JSON messages (max 1MB via `MaxControlMessageSize`)
- **Tunnel streams**: Stream 1+, raw TCP proxy data
- **BiProxy utility**: `protocol.BiProxy(conn1, conn2)` handles bidirectional copying with proper shutdown

Message flow:
1. Host sends `init` with env vars and agent forwarding config
2. Endpoint sends `port_update` when listening ports change
3. Host opens new yamux stream for each tunnel connection
4. Stream header: 4-byte port number, 1-byte success response

Agent/credential forwarding (reverse direction, endpoint → host):
- Stream type markers: `StreamTypeAgent` (0x01) for SSH, `StreamTypeGPG` (0x02) for GPG, `StreamTypeGitCred` (0x03) for git credentials
- SSH: Endpoint creates socket at `protocol.SSHAgentSocketPath`, sets `SSH_AUTH_SOCK` env var
- GPG: Endpoint runs `gpgconf --list-dirs agent-socket` to find expected path (e.g., `~/.gnupg/S.gpg-agent`), kills existing agent, creates socket there
- Git: Endpoint creates socket at `protocol.GitCredentialSocket`, creates helper script at `protocol.GitCredentialHelperPath`, configures git to use it
- On client connect: endpoint opens yamux stream to host with type marker, host proxies to local agent socket (SSH/GPG) or runs `git credential` command (git)

Shared constants (`internal/protocol/constants.go`):
- `SSHAgentSocketPath`, `GPGAgentSocketPath`, `GitCredentialSocket`, `GitCredentialHelperPath`
- `MaxControlMessageSize` (1MB), `MaxGitCredentialInput` (64KB)

### Host Binary (`cmd/dworm/`)

Entry point uses Cobra with subcommands: `up`, `down`, `shell`, `status`

Key flows:
- `up`: DevcontainerUp → InjectAndStart → SendInit → handle port updates → ForwardPort
- `down`: DevcontainerDown (finds container by label, docker stop)

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
- Local address is `IP:PORT` in hex
- State `0A` = TCP_LISTEN
- Filters to ports 1024-20000

### Tunnel Manager (`internal/host/tunnel.go`)

For each detected port:
1. Listen on `127.0.0.1:<port>`
2. On connection: open yamux stream, send port header, proxy bidirectionally

## Build

```bash
make build          # Both binaries
make build-host     # Host only (native)
make build-endpoint # Endpoint only (linux/amd64)
make fmt            # Format code
make tidy           # Update go.mod
```

Endpoint is always built for `GOOS=linux GOARCH=amd64` since it runs in containers.

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
- `test-ssh-agent.sh` - SSH agent forwarding (conditional - skips if no agent)
- `test-gpg-agent.sh` - GPG agent forwarding (conditional)
- `test-git-creds.sh` - Git credential forwarding (conditional)

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

## Error Handling Patterns

- Host logs to stderr with `[host]` prefix
- Endpoint logs to stderr with `[endpoint]` prefix (visible on host via docker exec stderr)
- Tunnel manager logs with `[tunnel]` prefix
- Control channel EOF is normal on shutdown
