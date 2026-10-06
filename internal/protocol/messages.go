package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Message types
const (
	TypeInit             = "init"
	TypePortUpdate       = "port_update"
	TypePing             = "ping"
	TypePong             = "pong"
	TypeEnvironment      = "environment"
	TypeEnvironmentReady = "environment_ready"
	TypeInitError        = "init_error"
	TypeLog              = "log" // endpoint → host: LogMessage
)

// ProtocolVersion is bumped on incompatible bridge changes. Host and endpoint
// come from the same build (the endpoint is embedded and injected on every
// `dworm up`); the check makes any mismatch fail loudly instead of corrupting
// streams.
//
// Version 2: host-opened streams start with a stream type marker.
// Version 3: endpoint log messages travel over the control channel (TypeLog).
const ProtocolVersion = 3

// Stream type markers (first byte of every yamux stream except the control stream)
const (
	// Opened by the endpoint
	StreamTypeAgent   byte = 0x01 // SSH agent forwarding stream
	StreamTypeGPG     byte = 0x02 // GPG agent forwarding stream
	StreamTypeGitCred byte = 0x03 // Git credential forwarding stream

	// Opened by the host
	StreamTypeTunnel byte = 0x10 // Port tunnel; followed by a 4-byte port header
	StreamTypeExec   byte = 0x11 // Process execution; see exec.go
)

// Message is the base envelope for all protocol messages
type Message struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
}

// InitMessage is sent from host to endpoint at startup
type InitMessage struct {
	EnvVars          map[string]string `json:"env_vars"`
	AgentForward     bool              `json:"agent_forward"`                // Whether to enable SSH agent forwarding
	AgentSocketPath  string            `json:"agent_socket_path,omitempty"`  // Path for agent socket in container
	GPGForward       bool              `json:"gpg_forward"`                  // Whether to enable GPG agent forwarding
	GPGSocketPath    string            `json:"gpg_socket_path,omitempty"`    // Path for GPG agent socket in container
	GitConfigContent string            `json:"git_config_content,omitempty"` // Content of host's ~/.gitconfig
	GitCredForward   bool              `json:"git_cred_forward"`             // Whether to enable git credential forwarding
	GPGPublicKeys    string            `json:"gpg_public_keys,omitempty"`    // Armored GPG public keys to import
	ProtocolVersion  int               `json:"protocol_version"`             // Must equal the endpoint's ProtocolVersion
}

// EnvironmentReadyMessage acknowledges init with the endpoint's protocol version.
type EnvironmentReadyMessage struct {
	ProtocolVersion int `json:"protocol_version"`
}

// InitErrorMessage reports why the endpoint rejected init.
type InitErrorMessage struct {
	Error string `json:"error"`
}

// Log levels of LogMessage.
const (
	LogLevelInfo  = "info"
	LogLevelWarn  = "warn"
	LogLevelError = "error"
)

// LogMessage is an endpoint log line, sent after environment_ready.
type LogMessage struct {
	Level   string `json:"level"`
	Message string `json:"message"`
}

// InferLogLevel classifies a free-form log line.
func InferLogLevel(line string) string {
	lower := strings.ToLower(line)
	switch {
	case strings.Contains(lower, "warning"):
		return LogLevelWarn
	case strings.Contains(lower, "error"), strings.Contains(lower, "failed"), strings.Contains(lower, "panic"):
		return LogLevelError
	default:
		return LogLevelInfo
	}
}

// PortInfo represents a listening port with its bind address
type PortInfo struct {
	Port    int    `json:"port"`
	Address string `json:"address"` // The bind address (e.g., "127.0.0.1", "::1", "0.0.0.0")
}

// PortUpdateMessage is sent from endpoint to host when ports change
type PortUpdateMessage struct {
	Ports []PortInfo `json:"ports"`
}

// EncodeMessage wraps a typed message in the envelope format
func EncodeMessage(msgType string, data interface{}) ([]byte, error) {
	var dataBytes json.RawMessage
	if data != nil {
		var err error
		dataBytes, err = json.Marshal(data)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal message data: %w", err)
		}
	}

	msg := Message{
		Type: msgType,
		Data: dataBytes,
	}

	return json.Marshal(msg)
}

// DecodeMessage reads the envelope and returns the type and raw data
func DecodeMessage(data []byte) (string, json.RawMessage, error) {
	var msg Message
	if err := json.Unmarshal(data, &msg); err != nil {
		return "", nil, fmt.Errorf("failed to unmarshal message envelope: %w", err)
	}
	return msg.Type, msg.Data, nil
}

// DecodeInit extracts an InitMessage from raw data
func DecodeInit(data json.RawMessage) (*InitMessage, error) {
	var msg InitMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, err
	}
	return &msg, nil
}

// DecodePortUpdate extracts a PortUpdateMessage from raw data
func DecodePortUpdate(data json.RawMessage) (*PortUpdateMessage, error) {
	var msg PortUpdateMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, err
	}
	return &msg, nil
}
