# Planned Features

## Protocol-based Logging

**Status:** Planned

**Problem:**

Currently, `dworm_endpoint` writes log messages to stderr because stdout is reserved for the yamux binary protocol. This works but has drawbacks:

1. Semantically incorrect - stderr is meant for errors, not informational messages
2. Cannot be easily filtered or controlled by the host
3. May be redirected unexpectedly by the user
4. Mixes with actual errors from docker exec
5. No way to implement verbosity levels or quiet mode

**Proposed Solution:**

Send log messages through the yamux control channel as a new message type:

```go
// New message type in internal/protocol/messages.go
const TypeLog = "log"

type LogMessage struct {
    Level   string `json:"level"`   // "debug", "info", "warn", "error"
    Message string `json:"message"`
}
```

**Benefits:**

- Host has full control over how/when/where to display logs
- Can implement `--quiet` and `--verbose` flags
- Can detect terminal capabilities on host and format appropriately (colors, etc.)
- Clean separation: endpoint reports data, host handles presentation
- No "side channel" communication via stderr

**Implementation Steps:**

1. Add `TypeLog` constant and `LogMessage` struct to `internal/protocol/messages.go`
2. Create a custom `io.Writer` in the endpoint that sends log messages over the control channel
3. Update endpoint's logger to use this writer instead of stderr
4. Add log message handling in the host's control message loop
5. Implement display logic with optional verbosity flags
6. Remove stderr forwarding from endpoint process (`e.cmd.Stderr = os.Stderr`)
