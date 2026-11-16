# Known Issues

This document tracks known limitations and potential issues with existing functionality. These are not bugs per se, but areas where the implementation could be more robust.

## TUI Architecture

### bubbleterm/bubbletea v2 incompatibility

**Issue**: The original plan to migrate to Bubble Tea + bubbleterm for terminal embedding was blocked because bubbleterm required bubbletea v2 (beta), which had compilation errors with the charmbracelet/x/ansi versions of the time. bubbletea v2 is stable now (`charm.land/bubbletea/v2`).

**Current implementation**: Uses lipgloss for status bar styling and retains the 3-goroutine terminal handling architecture (output reader, input handler, event handler) with direct PTY I/O. Child output passes through to the host terminal; a shadow screen (`x/vt`, the emulator bubbleterm also uses) supplies repaints and cursor state (see below).

**Future path**: Render everything from the emulator (bubbleterm). This would enable damage tracking (partial screen updates) which improves SSH performance, at the cost of passing nothing through: whatever the emulator does not implement would be lost, and keys would be re-encoded instead of forwarded as bytes.

**Files affected**: `internal/host/tui/model.go`, `internal/host/tui/statusbar.go`

### Shadow screen limits

The TUI repaints the shell area from a terminal emulator (`charmbracelet/x/vt`, see
`internal/host/tui/shadow.go`) when an application leaves its alternate screen, and uses it to put
the cursor back after drawing the status bar. Where the emulator and the host terminal disagree, the
repaint or the cursor can be off until the application redraws:

- Sequences the emulator does not implement reach the host terminal but are missing from a
  repaint.
- When an application has saved a cursor (ESC 7) and the status bar is redrawn, the cursor returns
  via CUP; a pending wrap at the right margin and the saved character set are lost. With left/right
  margins enabled (mode 69), `CSI s` is not tracked as a cursor save.
- A panic in the emulator resets the shadow screen (the session continues); the next repaint is
  then empty.
- Output floods: the emulator handles about 4 MB/s of printed text. dworm skips what scrolls off and
  batches scrolling, so floods reach 20–60 MB/s of TUI throughput, but plain-text floods are
  somewhat slower than without the shadow screen.

## Command Execution

### `dworm exec --no-bridge` can leave processes behind

**Issue**: `--no-bridge` is the only remaining `docker exec` path. Killing the
`docker exec` client does not signal or kill the process inside the container.
`dworm exec` forwards SIGINT/SIGTERM to the docker CLI, but if dworm itself is
killed (e.g. SIGKILL), the only remaining cleanup path is stdin EOF, which only
works for processes that exit on EOF.

**Current behavior**: Processes that ignore stdin EOF can survive their caller
on the `--no-bridge` path. Everything else (`dworm exec`, `dworm shell`, the TUI,
socket clients) runs over the instance, whose endpoint terminates the process
group, or hangs up the TTY session, when the caller disconnects.

### Raw exec clients: disconnect detection is Linux-only

**Issue**: In raw mode, a client half-close means stdin EOF, so dworm must tell a
half-close from a full disconnect. It uses `POLLHUP` on the unix socket, which
Linux reports only after a full close. On other host platforms a raw client that
disconnects after half-closing is noticed only when writing output fails or the
process exits. Framed mode is unaffected.

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
