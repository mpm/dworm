package protocol

import (
	"encoding/json"
	"fmt"
)

// Message types
const (
	TypeInit          = "init"
	TypePortUpdate    = "port_update"
	TypeTunnelRequest = "tunnel_request"
	TypeTunnelReady   = "tunnel_ready"
	TypePing          = "ping"
	TypePong          = "pong"
)

// Stream type markers (first byte of streams opened by endpoint)
const (
	StreamTypeAgent byte = 0x01 // SSH agent forwarding stream
	StreamTypeGPG   byte = 0x02 // GPG agent forwarding stream
)

// Message is the base envelope for all protocol messages
type Message struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
}

// InitMessage is sent from host to endpoint at startup
type InitMessage struct {
	EnvVars         map[string]string `json:"env_vars"`
	AgentForward    bool              `json:"agent_forward"`               // Whether to enable SSH agent forwarding
	AgentSocketPath string            `json:"agent_socket_path,omitempty"` // Path for agent socket in container
	GPGForward      bool              `json:"gpg_forward"`                 // Whether to enable GPG agent forwarding
	GPGSocketPath   string            `json:"gpg_socket_path,omitempty"`   // Path for GPG agent socket in container
}

// PortUpdateMessage is sent from endpoint to host when ports change
type PortUpdateMessage struct {
	Ports []int `json:"ports"`
}

// TunnelRequestMessage is sent from host to endpoint to request a tunnel
type TunnelRequestMessage struct {
	StreamID uint32 `json:"stream_id"`
	Port     int    `json:"port"`
}

// TunnelReadyMessage is sent from endpoint to host when tunnel is ready
type TunnelReadyMessage struct {
	StreamID uint32 `json:"stream_id"`
	Success  bool   `json:"success"`
	Error    string `json:"error,omitempty"`
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

// DecodeTunnelRequest extracts a TunnelRequestMessage from raw data
func DecodeTunnelRequest(data json.RawMessage) (*TunnelRequestMessage, error) {
	var msg TunnelRequestMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, err
	}
	return &msg, nil
}

// DecodeTunnelReady extracts a TunnelReadyMessage from raw data
func DecodeTunnelReady(data json.RawMessage) (*TunnelReadyMessage, error) {
	var msg TunnelReadyMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, err
	}
	return &msg, nil
}
