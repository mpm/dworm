package protocol

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
)
