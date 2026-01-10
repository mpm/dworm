# Known Issues

This document tracks known limitations and potential issues with existing functionality. These are not bugs per se, but areas where the implementation could be more robust.

## GPG Agent Forwarding

### gpg-agent auto-respawn

**Issue**: Some container setups have gpg-agent configured to auto-start via systemd user units, shell rc files, or GPG's built-in auto-spawn mechanism. If any GPG command runs inside the container, it may spawn a new local gpg-agent that takes over the socket, breaking forwarding.

**Current behavior**: We kill the agent once at startup with `gpgconf --kill gpg-agent`, but don't prevent respawn.

**Potential fixes**:
- Create `~/.gnupg/gpg-agent.conf` with `no-autostart` option
- Disable any systemd user units for gpg-agent
- Periodically monitor and re-kill competing agents

### Race condition on socket creation

**Issue**: There's a window between killing an existing gpg-agent and creating our forwarding socket where another process could spawn a new agent.

**Current behavior**: We kill, remove socket, then create our listener. No locking mechanism.

**Potential fixes**:
- Use file locking around socket creation
- Retry logic if socket creation fails

### Missing gpgconf in container

**Issue**: If gpgconf isn't installed in the container, we can't determine the correct socket path or kill an existing agent.

**Current behavior**: Falls back to the path provided in init message (`protocol.GPGAgentSocketPath`), which GPG won't find automatically. The kill command fails silently.

**Potential fixes**:
- Detect common socket paths (`~/.gnupg/S.gpg-agent`) as fallback
- Warn the user that GPG forwarding may not work without gpgconf
- Document that gpg must be installed in the container for forwarding to work

## Resolved Issues

### GetContainerID side effects (Fixed)

**Issue**: `host.GetContainerID()` previously ran `devcontainer up` which could restart or recreate containers.

**Resolution**: Changed to use Docker label query (`docker ps -q --filter label=devcontainer.local_folder=...`) for read-only container lookup.

### Bidirectional proxy race condition (Fixed)

**Issue**: Bidirectional data copying only waited for one goroutine to complete, causing potential data loss.

**Resolution**: Created `protocol.BiProxy()` utility that correctly waits for both directions to complete before returning.

### UpdatePorts race condition (Fixed)

**Issue**: `TunnelManager.UpdatePorts()` released the lock before accessing the listeners map.

**Resolution**: Refactored to hold lock for entire operation using internal `forwardPortLocked()` method.
