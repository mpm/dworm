# August Fixes

This document tracks the networking reliability work identified during the August 2026 port-forwarding investigation. The observed symptom is that host requests to a development server in the container can stall indefinitely after the connection has been idle or the server has been restarted.

## 1. Fix Tunnel Shutdown and Half-Close Handling

**Priority:** Critical

**Status:** Done

`protocol.BiProxy` propagates EOF with `CloseWrite`, but `*yamux.Stream` does not implement `CloseWrite`. It then waits for both copy directions before returning and closing the stream. When either TCP peer closes first, the remaining copy can stay blocked indefinitely.

This is the strongest explanation for stale browser connections, leaked streams, and gradual degradation after idle periods or server restarts.

**Files:**

- `internal/protocol/proxy.go`
- `internal/host/tunnel.go`
- `internal/endpoint/server.go`

**Work:**

- Define correct shutdown semantics for TCP-to-yamux proxying.
- Ensure EOF on either TCP side is propagated through yamux.
- Ensure errors terminate or unblock the opposite copy direction.
- Preserve graceful TCP half-close behavior where supported.
- Return or log copy errors where they help diagnose failures.

**Acceptance criteria:**

- Closing either side does not leave proxy goroutines or yamux streams blocked.
- A close-delimited HTTP response reaches the host completely.
- A client `CloseWrite` reaches the container service as EOF.
- Idle connection closure does not leave a browser with a silently stale tunnel.
- Repeated short connections return stream and goroutine counts to a stable baseline.

## 2. Synchronize Endpoint Port Address State

**Priority:** High

**Status:** Done

The scanner goroutine replaces and populates `Server.portAddresses` while tunnel-handler goroutines read it without synchronization. This is a data race and can expose empty, partial, or stale routing state. A fatal concurrent-map failure would also kill the endpoint.

**File:** `internal/endpoint/server.go`

**Work:**

- Protect the address map with a mutex or publish immutable snapshots atomically.
- Keep each tunnel lookup internally consistent with one completed scan.
- Add a race test that updates addresses while opening tunnel streams.

**Acceptance criteria:**

- Concurrent scans and tunnel connections pass under `go test -race`.
- Tunnel handlers never observe a partially populated address map.

## 3. Handle Endpoint and Transport Failure

**Priority:** High

When the endpoint process or yamux control channel fails, the host control goroutine only logs and exits. Host listeners remain active and continue accepting connections even though forwarding is unavailable.

**Files:**

- `cmd/dworm/main.go`
- `internal/host/endpoint.go`
- `internal/host/tunnel.go`

**Work:**

- Propagate endpoint/control-channel termination to the main lifecycle.
- Stop accepting local connections when the transport is unavailable.
- Decide whether the first implementation should terminate clearly or reconnect automatically.
- If reconnecting, recreate endpoint state and resend the complete current configuration.
- Surface transport failures outside the TUI log buffer where appropriate.

**Acceptance criteria:**

- Killing the endpoint cannot leave apparently live but unusable listeners indefinitely.
- `dworm up --daemon` either restores forwarding or exits with a clear error.
- Interactive mode visibly reports loss of forwarding.

## 4. Add Tunnel Establishment Deadlines

**Priority:** High

**Status:** Done

Tunnel setup has no end-to-end deadline. The host can wait indefinitely for the endpoint response, and the endpoint uses an unbounded `net.Dial` call.

**Files:**

- `internal/host/endpoint.go`
- `internal/endpoint/server.go`

**Work:**

- Add a bounded deadline for the port header and success-response handshake.
- Use `net.Dialer` with a timeout for the container-side connection.
- Clear setup deadlines before starting normal proxy traffic.
- Check and report success/failure response write errors.

**Acceptance criteria:**

- A failed or wedged target connection closes the accepted host connection promptly.
- A missing endpoint response cannot stall indefinitely.
- Normal long-lived traffic is not constrained by the setup deadline.

## 5. Make Port Updates Recoverable

**Priority:** Medium

The endpoint records a new scan as current before confirming that its control message was sent. If sending fails, unchanged scans do not retry. Similarly, a transient host listener creation failure is only logged and is not retried until the container port set changes again.

**Files:**

- `internal/endpoint/server.go`
- `internal/host/tunnel.go`

**Work:**

- Commit `currentPorts` only after successful delivery, or retain a pending update for retry.
- Retry forwarding creation for ports that remain present but have no listener.
- Keep retries bounded or paced to avoid noisy loops.

**Acceptance criteria:**

- A transient control-send failure does not permanently lose a port update.
- A transient local bind failure is retried while the container port remains open.

## 6. Handle Port Scanner Errors Safely

**Priority:** Medium

Errors reading `/proc/net/tcp` and `/proc/net/tcp6` are currently ignored. An incomplete scan can be treated as a real removal and cause host listeners to be closed.

**File:** `internal/endpoint/portscanner.go`

**Work:**

- Distinguish an unavailable address family from an unexpected scan failure.
- Do not publish an empty or incomplete snapshot after a transient read error.
- Log actionable scan failures.

**Acceptance criteria:**

- A transient `/proc` read failure does not remove valid forwarded ports.
- IPv4-only and IPv6-only environments continue to work.

## 7. Close Active Tunnels When a Port Disappears

**Priority:** Medium

Removing a forwarded port closes only its host listener. Existing accepted connections remain active and may point to a stopped or replaced development server.

**File:** `internal/host/tunnel.go`

**Work:**

- Track active connections by container port.
- Close those connections when forwarding for the port is removed.
- Ensure normal connection completion unregisters them without races.

**Acceptance criteria:**

- Stopping a development server closes existing host tunnels for that port.
- Restarting the server cannot leave old and new tunnels mixed indefinitely.

## 8. Improve Yamux Diagnostics

**Priority:** Medium

Yamux logging is currently sent to `io.Discard`, hiding keepalive failures, protocol errors, backlog exhaustion, and stream-open failures.

**File:** `internal/protocol/mux.go`

**Work:**

- Route useful yamux diagnostics through the existing host and endpoint logging paths.
- Avoid excessive normal-operation logging.
- Consider exposing session close reasons through `protocol.Mux`.

**Acceptance criteria:**

- Transport-level failure has a visible diagnostic.
- Routine connection activity does not flood logs or the TUI.

## 9. Expand Networking Tests

**Priority:** Required alongside the fixes above

The current E2E test performs one request against a newly started server. There are no focused tests for `BiProxy`, `TunnelManager`, endpoint tunnel handling, idle connections, or restart behavior.

**Files:**

- `internal/protocol/proxy_test.go` (new)
- Host and endpoint networking tests as appropriate
- `test/e2e/test-port-forward.sh`

**Scenarios:**

- TCP client closes while the server remains open.
- Server closes while the client remains open.
- Client half-closes its write side and waits for a response.
- Close-delimited HTTP response.
- Repeated short connections without stream or goroutine growth.
- Idle keep-alive closure followed by a new request.
- Server stop and restart on the same port.
- Server restart both within and beyond one scan interval.
- Active connections during port removal.
- Endpoint process termination.
- Timeout while waiting for tunnel establishment.
- Failed port-update delivery followed by an unchanged scan.
- Concurrent address updates and tunnel establishment under the race detector.
- A session idle beyond yamux's keepalive interval.

## Suggested Order

1. Fix `BiProxy` shutdown semantics and add focused proxy tests.
2. Synchronize endpoint address state and verify it with the race detector.
3. Add setup deadlines.
4. Handle endpoint loss and stop zombie listeners.
5. Add update retries and safe scanner-error behavior.
6. Track and close active tunnels on port removal.
7. Improve yamux diagnostics.
8. Expand the Docker E2E test to cover idle connections and server restarts.

## Verification Commands

```bash
make test-unit
make test-race
make test-e2e
```

The Docker E2E suite should be run after lifecycle and restart handling changes. Unit and race tests should be run after every step.
