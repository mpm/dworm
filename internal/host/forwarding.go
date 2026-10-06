package host

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/mpm/dworm/internal/protocol"
)

// forwardingConfig is the host side of agent and credential forwarding,
// detected once when an instance starts.
type forwardingConfig struct {
	sshSocket     string // host SSH agent socket; "" disables SSH forwarding
	gpgSocket     string // host GPG agent socket; "" disables GPG forwarding
	gpgPublicKeys string
	gitConfig     string
	gitCred       bool
}

func (f forwardingConfig) any() bool {
	return f.sshSocket != "" || f.gpgSocket != "" || f.gitCred
}

// initMessage builds the endpoint init message for env.
func (f forwardingConfig) initMessage(env map[string]string) *protocol.InitMessage {
	msg := &protocol.InitMessage{
		EnvVars:          env,
		GPGPublicKeys:    f.gpgPublicKeys,
		GitConfigContent: f.gitConfig,
		GitCredForward:   f.gitCred,
		ProtocolVersion:  protocol.ProtocolVersion,
	}
	if f.sshSocket != "" {
		msg.AgentForward = true
		msg.AgentSocketPath = protocol.SSHAgentSocketPath
	}
	if f.gpgSocket != "" {
		msg.GPGForward = true
		msg.GPGSocketPath = protocol.GPGAgentSocketPath
	}
	return msg
}

// detectForwarding checks which host agents and credentials can be forwarded.
func detectForwarding(logger *log.Logger) forwardingConfig {
	var f forwardingConfig

	if socket := os.Getenv("SSH_AUTH_SOCK"); socket == "" {
		logger.Printf("Warning: SSH agent forwarding disabled (SSH_AUTH_SOCK not set)")
	} else if _, err := os.Stat(socket); err != nil {
		logger.Printf("Warning: SSH agent forwarding disabled (socket not found: %s)", socket)
	} else {
		f.sshSocket = socket
	}

	if socket, err := getGPGAgentSocket(); err != nil {
		logger.Printf("Warning: GPG agent forwarding disabled (%v)", err)
	} else {
		f.gpgSocket = socket
		if output, err := exec.Command("gpg", "--export", "--armor").Output(); err != nil {
			logger.Printf("Warning: failed to export GPG public keys: %v", err)
		} else if len(output) > 0 {
			f.gpgPublicKeys = string(output)
			logger.Printf("Exported GPG public keys (%d bytes)", len(output))
		}
	}

	if homeDir, err := os.UserHomeDir(); err == nil {
		if content, err := os.ReadFile(filepath.Join(homeDir, ".gitconfig")); err == nil {
			f.gitConfig = string(content)
			logger.Printf("Read host git config (%d bytes)", len(content))
		} else {
			logger.Printf("Warning: could not read host git config: %v", err)
		}
	}

	// Git credential forwarding is enabled if git is available on the host.
	if _, err := exec.LookPath("git"); err == nil {
		f.gitCred = true
	} else {
		logger.Printf("Warning: git credential forwarding disabled (git not found on host)")
	}
	return f
}

// getGPGAgentSocket returns the GPG agent socket path using gpgconf
func getGPGAgentSocket() (string, error) {
	output, err := exec.Command("gpgconf", "--list-dirs", "agent-socket").Output()
	if err != nil {
		return "", fmt.Errorf("gpgconf failed: %w", err)
	}
	socketPath := strings.TrimSpace(string(output))
	if socketPath == "" {
		return "", fmt.Errorf("gpgconf returned empty socket path")
	}
	if _, err := os.Stat(socketPath); err != nil {
		return "", fmt.Errorf("GPG agent socket does not exist: %s", socketPath)
	}
	return socketPath, nil
}
