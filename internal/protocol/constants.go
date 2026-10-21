package protocol

import "time"

// Socket paths for agent forwarding in the container
const (
	SSHAgentSocketPath      = "/tmp/dworm-ssh-agent.sock"
	GPGAgentSocketPath      = "/tmp/dworm-gpg-agent.sock"
	GitCredentialSocket     = "/tmp/dworm-git-credential.sock"
	GitCredentialHelperPath = "/tmp/dworm-git-credential"
)

// Protocol limits
const (
	MaxControlMessageSize = 1024 * 1024 // 1MB max control message size
	MaxGitCredentialInput = 64 * 1024   // 64KB max git credential input
	TunnelSetupTimeout    = 5 * time.Second
)

// BridgeStallTimeout is how long the bridge tolerates an unanswered keepalive
// ping, a blocked write, or an unacknowledged stream open before yamux closes
// the session. The transport is a local docker exec pipe whose death shows up
// as EOF at once, so these timeouts only catch a hung peer. A host stalled by
// memory pressure can freeze both sides for minutes, and yamux's defaults
// (10s, 75s for stream opens) then tear down the bridge and every exec on it.
const BridgeStallTimeout = 5 * time.Minute
