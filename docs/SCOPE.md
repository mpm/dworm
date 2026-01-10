# dworm — Development Wormhole

## Overview

dworm is a CLI tool that wraps the devcontainer CLI to provide seamless development environment bridging between a host machine and a containerized development environment. It replicates key features of VS Code's proprietary remote development extensions in a standalone, editor-agnostic way.

## Architecture

Two cooperating binaries:

- **dworm** (host side) — Orchestrates container lifecycle, injects the endpoint binary, maintains the communication channel, and exposes forwarded ports locally.
- **dworm_endpoint** (container side) — Runs inside the container, monitors for open ports, receives environment configuration, and configures the container environment.

## Communication

Host and endpoint communicate over a **multiplexed connection over stdin/stdout** of the endpoint process (launched via `docker exec` or similar). This avoids needing to expose additional ports or set up networking — the container just needs to be running, and dworm attaches to it.

Protocol: Length-prefixed JSON messages or a simple RPC protocol over the pipe, supporting bidirectional async communication.

## Core Features

### Port Forwarding (container → host)
- Endpoint scans for newly opened listening ports inside the container (polling `/proc/net/tcp` or similar)
- Reports port changes to host over the communication channel
- Host binds matching ports locally and tunnels traffic to container

### Environment Variable Injection (host → container)
- Specified via CLI flags: `dworm --env FOO=bar --env BAZ`
- Transmitted to endpoint at startup
- Endpoint sets these in its own environment and makes them available to child processes / shells

### Credential Forwarding
- **SSH agent**: Forward `SSH_AUTH_SOCK` by proxying the Unix socket over the communication channel
- **GPG agent**: Similar socket forwarding for `~/.gnupg/S.gpg-agent`
- **Git config**: Transmit `user.name`, `user.email`, and optionally `user.signingkey` to be written to `~/.gitconfig` inside the container

### Operating Modes
1. **Shell mode** (default): Opens an interactive shell in the container with all forwarding active
2. **Daemon mode** (`--daemon`): Sits in foreground, prints port/config changes to stdout, useful for integration with other tools or editors

## CLI Interface (draft)

```
dworm up [devcontainer-path]     # start container, inject endpoint, establish tunnel
dworm down                       # stop container
dworm shell                      # open shell (default action after up)
dworm status                     # show forwarded ports, active config
dworm --env KEY=VALUE            # inject environment variable
dworm --forward-ssh              # enable SSH agent forwarding
dworm --forward-gpg              # enable GPG agent forwarding
dworm --forward-git              # copy git user.name/email into container
```

## Non-Goals (v1)

- Clipboard synchronization
- File watching / filesystem events
- Extension management
- Settings sync

## Open Questions

1. Should port forwarding be opt-in per port, or all-by-default with exclusions?
   -> For now it should exclude obvious system services, but auto
      redirect all ports from lets say 1000-20000?
2. Should dworm manage its own container lifecycle or strictly wrap devcontainer CLI?
   -> If there is something that cannot be done with the devcontainer
      CLI (like special params for mounting etc.?) it has to call docker
      directly. However, the vision is that the tool is flexible enough so
      that it also works the same way with any remote location that is
      reachable via SSH (so not only containers where it can connect to
      directly. However, this is explicitly not part of v1)
3. How to handle reconnection if the communication channel drops?
   -> If it calls exec for the container, it should check if a
      dworm_endpoint is already running inside the container. If so, it can
      just re-attach and re-initialize everything.
