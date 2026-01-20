package protocol_test

import (
	"bytes"
	"encoding/json"
	"io"
	"sync"
	"testing"

	"github.com/mpm/dworm/internal/protocol"
	"github.com/mpm/dworm/internal/protocol/testutil"
)

func TestMuxControlMessage(t *testing.T) {
	h, err := testutil.NewTestHarness()
	if err != nil {
		t.Fatalf("failed to create harness: %v", err)
	}
	defer h.Close()

	// Send init message from host
	initMsg := &protocol.InitMessage{
		EnvVars:      map[string]string{"FOO": "bar", "BAZ": "qux"},
		AgentForward: true,
	}

	if err := h.HostMux.SendControl(protocol.TypeInit, initMsg); err != nil {
		t.Fatalf("failed to send control message: %v", err)
	}

	// Receive on endpoint
	msgType, data, err := h.EndpointMux.RecvControl()
	if err != nil {
		t.Fatalf("failed to receive control message: %v", err)
	}

	if msgType != protocol.TypeInit {
		t.Errorf("expected message type %q, got %q", protocol.TypeInit, msgType)
	}

	received, err := protocol.DecodeInit(data)
	if err != nil {
		t.Fatalf("failed to decode init message: %v", err)
	}

	if received.EnvVars["FOO"] != "bar" {
		t.Errorf("expected FOO=bar, got FOO=%s", received.EnvVars["FOO"])
	}
	if !received.AgentForward {
		t.Error("expected AgentForward to be true")
	}
}

func TestMuxBidirectionalControl(t *testing.T) {
	h, err := testutil.NewTestHarness()
	if err != nil {
		t.Fatalf("failed to create harness: %v", err)
	}
	defer h.Close()

	// Send port update from endpoint to host
	portMsg := &protocol.PortUpdateMessage{
		Ports: []protocol.PortInfo{
			{Port: 8080, Address: "0.0.0.0"},
			{Port: 3000, Address: "127.0.0.1"},
			{Port: 5432, Address: "::1"},
		},
	}

	if err := h.EndpointMux.SendControl(protocol.TypePortUpdate, portMsg); err != nil {
		t.Fatalf("failed to send port update: %v", err)
	}

	// Receive on host
	msgType, data, err := h.HostMux.RecvControl()
	if err != nil {
		t.Fatalf("failed to receive port update: %v", err)
	}

	if msgType != protocol.TypePortUpdate {
		t.Errorf("expected message type %q, got %q", protocol.TypePortUpdate, msgType)
	}

	received, err := protocol.DecodePortUpdate(data)
	if err != nil {
		t.Fatalf("failed to decode port update: %v", err)
	}

	if len(received.Ports) != 3 {
		t.Fatalf("expected 3 ports, got %d", len(received.Ports))
	}
	if received.Ports[0].Port != 8080 || received.Ports[1].Port != 3000 || received.Ports[2].Port != 5432 {
		t.Errorf("ports mismatch: got %v", received.Ports)
	}
}

func TestMuxMultipleControlMessages(t *testing.T) {
	h, err := testutil.NewTestHarness()
	if err != nil {
		t.Fatalf("failed to create harness: %v", err)
	}
	defer h.Close()

	// Send multiple messages in sequence
	for i := 0; i < 5; i++ {
		if err := h.HostMux.SendControl(protocol.TypePing, nil); err != nil {
			t.Fatalf("failed to send ping %d: %v", i, err)
		}

		msgType, _, err := h.EndpointMux.RecvControl()
		if err != nil {
			t.Fatalf("failed to receive ping %d: %v", i, err)
		}

		if msgType != protocol.TypePing {
			t.Errorf("expected ping, got %s", msgType)
		}
	}
}

func TestMuxStream(t *testing.T) {
	h, err := testutil.NewTestHarness()
	if err != nil {
		t.Fatalf("failed to create harness: %v", err)
	}
	defer h.Close()

	testData := []byte("hello from host")
	responseData := []byte("hello from endpoint")

	var wg sync.WaitGroup
	wg.Add(2)

	// Host opens stream and sends data
	go func() {
		defer wg.Done()
		stream, err := h.HostMux.OpenStream()
		if err != nil {
			t.Errorf("host: failed to open stream: %v", err)
			return
		}
		defer stream.Close()

		if _, err := stream.Write(testData); err != nil {
			t.Errorf("host: failed to write: %v", err)
			return
		}

		// Read response
		buf := make([]byte, len(responseData))
		if _, err := io.ReadFull(stream, buf); err != nil {
			t.Errorf("host: failed to read response: %v", err)
			return
		}

		if !bytes.Equal(buf, responseData) {
			t.Errorf("host: expected %q, got %q", responseData, buf)
		}
	}()

	// Endpoint accepts stream and echoes
	go func() {
		defer wg.Done()
		stream, err := h.EndpointMux.AcceptStream()
		if err != nil {
			t.Errorf("endpoint: failed to accept stream: %v", err)
			return
		}
		defer stream.Close()

		buf := make([]byte, len(testData))
		if _, err := io.ReadFull(stream, buf); err != nil {
			t.Errorf("endpoint: failed to read: %v", err)
			return
		}

		if !bytes.Equal(buf, testData) {
			t.Errorf("endpoint: expected %q, got %q", testData, buf)
		}

		// Send response
		if _, err := stream.Write(responseData); err != nil {
			t.Errorf("endpoint: failed to write response: %v", err)
		}
	}()

	wg.Wait()
}

func TestMuxMultipleStreams(t *testing.T) {
	h, err := testutil.NewTestHarness()
	if err != nil {
		t.Fatalf("failed to create harness: %v", err)
	}
	defer h.Close()

	const numStreams = 5
	var wg sync.WaitGroup
	wg.Add(numStreams * 2)

	// Open multiple streams from host
	for i := 0; i < numStreams; i++ {
		streamID := byte(i)

		go func() {
			defer wg.Done()
			stream, err := h.HostMux.OpenStream()
			if err != nil {
				t.Errorf("host: failed to open stream %d: %v", streamID, err)
				return
			}
			defer stream.Close()

			// Send stream ID
			if _, err := stream.Write([]byte{streamID}); err != nil {
				t.Errorf("host: failed to write stream %d: %v", streamID, err)
				return
			}

			// Read echoed ID
			buf := make([]byte, 1)
			if _, err := io.ReadFull(stream, buf); err != nil {
				t.Errorf("host: failed to read stream %d: %v", streamID, err)
				return
			}

			if buf[0] != streamID {
				t.Errorf("host: stream %d got wrong echo: %d", streamID, buf[0])
			}
		}()
	}

	// Accept and echo on endpoint
	for i := 0; i < numStreams; i++ {
		go func() {
			defer wg.Done()
			stream, err := h.EndpointMux.AcceptStream()
			if err != nil {
				t.Errorf("endpoint: failed to accept stream: %v", err)
				return
			}
			defer stream.Close()

			buf := make([]byte, 1)
			if _, err := io.ReadFull(stream, buf); err != nil {
				t.Errorf("endpoint: failed to read: %v", err)
				return
			}

			// Echo back
			if _, err := stream.Write(buf); err != nil {
				t.Errorf("endpoint: failed to write: %v", err)
			}
		}()
	}

	wg.Wait()
}

func TestMuxLargeMessage(t *testing.T) {
	h, err := testutil.NewTestHarness()
	if err != nil {
		t.Fatalf("failed to create harness: %v", err)
	}
	defer h.Close()

	// Create a moderately large init message (within yamux window size)
	largeEnvVars := make(map[string]string)
	// Create ~50KB of env vars (well under yamux 256KB window)
	for i := 0; i < 100; i++ {
		key := bytes.Repeat([]byte{'K'}, 50)
		value := bytes.Repeat([]byte{'V'}, 450)
		largeEnvVars[string(key)+string(rune(i))] = string(value)
	}

	initMsg := &protocol.InitMessage{
		EnvVars: largeEnvVars,
	}

	var wg sync.WaitGroup
	var recvMsgType string
	var recvData []byte
	var recvErr error

	wg.Add(1)
	// Start receiver in goroutine to avoid deadlock
	go func() {
		defer wg.Done()
		recvMsgType, recvData, recvErr = h.EndpointMux.RecvControl()
	}()

	if err := h.HostMux.SendControl(protocol.TypeInit, initMsg); err != nil {
		t.Fatalf("failed to send large message: %v", err)
	}

	wg.Wait()

	if recvErr != nil {
		t.Fatalf("failed to receive large message: %v", recvErr)
	}

	if recvMsgType != protocol.TypeInit {
		t.Errorf("expected init, got %s", recvMsgType)
	}

	received, err := protocol.DecodeInit(recvData)
	if err != nil {
		t.Fatalf("failed to decode: %v", err)
	}

	if len(received.EnvVars) != len(largeEnvVars) {
		t.Errorf("expected %d env vars, got %d", len(largeEnvVars), len(received.EnvVars))
	}
}

func TestMuxInitMessageFields(t *testing.T) {
	h, err := testutil.NewTestHarness()
	if err != nil {
		t.Fatalf("failed to create harness: %v", err)
	}
	defer h.Close()

	// Test all fields of InitMessage
	initMsg := &protocol.InitMessage{
		EnvVars:          map[string]string{"HOME": "/home/test"},
		AgentForward:     true,
		AgentSocketPath:  "/tmp/ssh.sock",
		GPGForward:       true,
		GPGSocketPath:    "/tmp/gpg.sock",
		GitConfigContent: "[user]\n\tname = Test",
		GitCredForward:   true,
	}

	if err := h.HostMux.SendControl(protocol.TypeInit, initMsg); err != nil {
		t.Fatalf("failed to send: %v", err)
	}

	_, data, err := h.EndpointMux.RecvControl()
	if err != nil {
		t.Fatalf("failed to receive: %v", err)
	}

	received, err := protocol.DecodeInit(data)
	if err != nil {
		t.Fatalf("failed to decode: %v", err)
	}

	if received.EnvVars["HOME"] != "/home/test" {
		t.Error("HOME env var mismatch")
	}
	if !received.AgentForward {
		t.Error("AgentForward should be true")
	}
	if received.AgentSocketPath != "/tmp/ssh.sock" {
		t.Error("AgentSocketPath mismatch")
	}
	if !received.GPGForward {
		t.Error("GPGForward should be true")
	}
	if received.GPGSocketPath != "/tmp/gpg.sock" {
		t.Error("GPGSocketPath mismatch")
	}
	if received.GitConfigContent != "[user]\n\tname = Test" {
		t.Error("GitConfigContent mismatch")
	}
	if !received.GitCredForward {
		t.Error("GitCredForward should be true")
	}
}

func TestMuxSessionClose(t *testing.T) {
	h, err := testutil.NewTestHarness()
	if err != nil {
		t.Fatalf("failed to create harness: %v", err)
	}

	// Close host mux
	h.HostMux.Close()

	// Verify it's closed
	if !h.HostMux.IsClosed() {
		t.Error("host mux should be closed")
	}

	// Endpoint should eventually see the closure
	_, _, err = h.EndpointMux.RecvControl()
	if err == nil {
		t.Error("expected error after close, got nil")
	}

	h.EndpointMux.Close()
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	tests := []struct {
		name    string
		msgType string
		data    interface{}
	}{
		{
			name:    "init message",
			msgType: protocol.TypeInit,
			data: &protocol.InitMessage{
				EnvVars:      map[string]string{"A": "B"},
				AgentForward: true,
			},
		},
		{
			name:    "port update",
			msgType: protocol.TypePortUpdate,
			data:    &protocol.PortUpdateMessage{Ports: []protocol.PortInfo{{Port: 8080, Address: "0.0.0.0"}, {Port: 3000, Address: "127.0.0.1"}}},
		},
		{
			name:    "ping",
			msgType: protocol.TypePing,
			data:    nil,
		},
		{
			name:    "pong",
			msgType: protocol.TypePong,
			data:    nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoded, err := protocol.EncodeMessage(tt.msgType, tt.data)
			if err != nil {
				t.Fatalf("encode failed: %v", err)
			}

			msgType, rawData, err := protocol.DecodeMessage(encoded)
			if err != nil {
				t.Fatalf("decode failed: %v", err)
			}

			if msgType != tt.msgType {
				t.Errorf("message type mismatch: expected %q, got %q", tt.msgType, msgType)
			}

			// Verify data can be unmarshaled if present
			if tt.data != nil && len(rawData) > 0 {
				var temp interface{}
				if err := json.Unmarshal(rawData, &temp); err != nil {
					t.Errorf("raw data not valid JSON: %v", err)
				}
			}
		})
	}
}
