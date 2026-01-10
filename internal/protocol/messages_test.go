package protocol

import (
	"encoding/json"
	"testing"
)

func TestEncodeMessage(t *testing.T) {
	tests := []struct {
		name     string
		msgType  string
		data     interface{}
		wantErr  bool
		checkFn  func(t *testing.T, encoded []byte)
	}{
		{
			name:    "init message",
			msgType: TypeInit,
			data: &InitMessage{
				EnvVars:      map[string]string{"FOO": "bar"},
				AgentForward: true,
			},
			checkFn: func(t *testing.T, encoded []byte) {
				var msg Message
				if err := json.Unmarshal(encoded, &msg); err != nil {
					t.Fatalf("unmarshal failed: %v", err)
				}
				if msg.Type != TypeInit {
					t.Errorf("type mismatch: got %q", msg.Type)
				}
			},
		},
		{
			name:    "port update",
			msgType: TypePortUpdate,
			data:    &PortUpdateMessage{Ports: []int{8080}},
		},
		{
			name:    "nil data",
			msgType: TypePing,
			data:    nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoded, err := EncodeMessage(tt.msgType, tt.data)
			if (err != nil) != tt.wantErr {
				t.Fatalf("EncodeMessage() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.checkFn != nil {
				tt.checkFn(t, encoded)
			}
		})
	}
}

func TestDecodeMessage(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantType    string
		wantErr     bool
		wantDataLen int
	}{
		{
			name:     "valid init",
			input:    `{"type":"init","data":{"env_vars":{}}}`,
			wantType: TypeInit,
		},
		{
			name:     "valid port update",
			input:    `{"type":"port_update","data":{"ports":[8080]}}`,
			wantType: TypePortUpdate,
		},
		{
			name:     "no data field",
			input:    `{"type":"ping"}`,
			wantType: TypePing,
		},
		{
			name:    "invalid json",
			input:   `{invalid}`,
			wantErr: true,
		},
		{
			name:    "empty input",
			input:   ``,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msgType, _, err := DecodeMessage([]byte(tt.input))
			if (err != nil) != tt.wantErr {
				t.Fatalf("DecodeMessage() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && msgType != tt.wantType {
				t.Errorf("DecodeMessage() type = %q, want %q", msgType, tt.wantType)
			}
		})
	}
}

func TestDecodeInit(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
		checkFn func(t *testing.T, msg *InitMessage)
	}{
		{
			name:  "full message",
			input: `{"env_vars":{"A":"B"},"agent_forward":true,"agent_socket_path":"/tmp/a","gpg_forward":true,"gpg_socket_path":"/tmp/g","git_config_content":"[user]","git_cred_forward":true}`,
			checkFn: func(t *testing.T, msg *InitMessage) {
				if msg.EnvVars["A"] != "B" {
					t.Error("env vars mismatch")
				}
				if !msg.AgentForward {
					t.Error("AgentForward should be true")
				}
				if msg.AgentSocketPath != "/tmp/a" {
					t.Error("AgentSocketPath mismatch")
				}
				if !msg.GPGForward {
					t.Error("GPGForward should be true")
				}
				if msg.GPGSocketPath != "/tmp/g" {
					t.Error("GPGSocketPath mismatch")
				}
				if msg.GitConfigContent != "[user]" {
					t.Error("GitConfigContent mismatch")
				}
				if !msg.GitCredForward {
					t.Error("GitCredForward should be true")
				}
			},
		},
		{
			name:  "minimal message",
			input: `{}`,
			checkFn: func(t *testing.T, msg *InitMessage) {
				if msg.AgentForward {
					t.Error("AgentForward should default to false")
				}
			},
		},
		{
			name:  "empty env vars",
			input: `{"env_vars":{}}`,
			checkFn: func(t *testing.T, msg *InitMessage) {
				if len(msg.EnvVars) != 0 {
					t.Error("expected empty env vars")
				}
			},
		},
		{
			name:    "invalid json",
			input:   `{invalid`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg, err := DecodeInit(json.RawMessage(tt.input))
			if (err != nil) != tt.wantErr {
				t.Fatalf("DecodeInit() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && tt.checkFn != nil {
				tt.checkFn(t, msg)
			}
		})
	}
}

func TestDecodePortUpdate(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
		checkFn func(t *testing.T, msg *PortUpdateMessage)
	}{
		{
			name:  "multiple ports",
			input: `{"ports":[8080,3000,5432]}`,
			checkFn: func(t *testing.T, msg *PortUpdateMessage) {
				if len(msg.Ports) != 3 {
					t.Fatalf("expected 3 ports, got %d", len(msg.Ports))
				}
				if msg.Ports[0] != 8080 || msg.Ports[1] != 3000 || msg.Ports[2] != 5432 {
					t.Errorf("ports mismatch: %v", msg.Ports)
				}
			},
		},
		{
			name:  "empty ports",
			input: `{"ports":[]}`,
			checkFn: func(t *testing.T, msg *PortUpdateMessage) {
				if len(msg.Ports) != 0 {
					t.Error("expected empty ports")
				}
			},
		},
		{
			name:  "single port",
			input: `{"ports":[8080]}`,
			checkFn: func(t *testing.T, msg *PortUpdateMessage) {
				if len(msg.Ports) != 1 || msg.Ports[0] != 8080 {
					t.Error("expected single port 8080")
				}
			},
		},
		{
			name:    "invalid json",
			input:   `{"ports":}`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg, err := DecodePortUpdate(json.RawMessage(tt.input))
			if (err != nil) != tt.wantErr {
				t.Fatalf("DecodePortUpdate() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && tt.checkFn != nil {
				tt.checkFn(t, msg)
			}
		})
	}
}

func TestStreamTypeConstants(t *testing.T) {
	// Verify stream type constants are distinct
	types := []byte{StreamTypeAgent, StreamTypeGPG, StreamTypeGitCred}
	seen := make(map[byte]bool)
	for _, typ := range types {
		if seen[typ] {
			t.Errorf("duplicate stream type: 0x%02x", typ)
		}
		seen[typ] = true
	}

	// Verify expected values
	if StreamTypeAgent != 0x01 {
		t.Errorf("StreamTypeAgent = 0x%02x, want 0x01", StreamTypeAgent)
	}
	if StreamTypeGPG != 0x02 {
		t.Errorf("StreamTypeGPG = 0x%02x, want 0x02", StreamTypeGPG)
	}
	if StreamTypeGitCred != 0x03 {
		t.Errorf("StreamTypeGitCred = 0x%02x, want 0x03", StreamTypeGitCred)
	}
}

func TestMessageTypeConstants(t *testing.T) {
	// Verify message type constants match expected values
	if TypeInit != "init" {
		t.Errorf("TypeInit = %q, want %q", TypeInit, "init")
	}
	if TypePortUpdate != "port_update" {
		t.Errorf("TypePortUpdate = %q, want %q", TypePortUpdate, "port_update")
	}
	if TypePing != "ping" {
		t.Errorf("TypePing = %q, want %q", TypePing, "ping")
	}
	if TypePong != "pong" {
		t.Errorf("TypePong = %q, want %q", TypePong, "pong")
	}
}
