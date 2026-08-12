package host

import (
	"bytes"
	"encoding/binary"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/mpm/dworm/internal/protocol"
	"github.com/mpm/dworm/internal/protocol/testutil"
)

func TestAgentHandlerSSH(t *testing.T) {
	// Create a temporary Unix socket for the mock SSH agent
	tmpDir, err := os.MkdirTemp("", "dworm-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	socketPath := filepath.Join(tmpDir, "ssh-agent.sock")

	// Create a mock SSH agent that echoes a single message
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("failed to create mock agent socket: %v", err)
	}
	defer listener.Close()

	testData := []byte("hello agent")

	// Start mock agent - reads exact amount, echoes, then closes
	mockAgentConn := make(chan net.Conn, 1)
	mockAgentDone := make(chan struct{})
	go func() {
		defer close(mockAgentDone)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		mockAgentConn <- conn
		buf := make([]byte, len(testData))
		io.ReadFull(conn, buf)
		conn.Write(buf)
	}()

	// Create test harness
	h, err := testutil.NewTestHarness()
	if err != nil {
		t.Fatalf("failed to create harness: %v", err)
	}
	defer h.Close()

	// Create agent handler
	logger := log.New(io.Discard, "", 0)
	handler := NewAgentHandler(h.HostMux, socketPath, logger)
	handler.Start()
	defer handler.Close()

	// Endpoint opens a stream with SSH agent type marker
	stream, err := h.EndpointMux.OpenStream()
	if err != nil {
		t.Fatalf("failed to open stream: %v", err)
	}
	defer stream.Close()

	// Send SSH agent type marker
	if _, err := stream.Write([]byte{protocol.StreamTypeAgent}); err != nil {
		t.Fatalf("failed to write stream type: %v", err)
	}

	// Read success response
	resp := make([]byte, 1)
	if _, err := io.ReadFull(stream, resp); err != nil {
		t.Fatalf("failed to read response: %v", err)
	}

	if resp[0] != 1 {
		t.Errorf("expected success (1), got %d", resp[0])
	}

	// Send test data through the tunnel
	if _, err := stream.Write(testData); err != nil {
		t.Fatalf("failed to write test data: %v", err)
	}

	// Read echoed data
	echoed := make([]byte, len(testData))
	if _, err := io.ReadFull(stream, echoed); err != nil {
		t.Fatalf("failed to read echoed data: %v", err)
	}

	if !bytes.Equal(testData, echoed) {
		t.Errorf("expected %q, got %q", testData, echoed)
	}

	// Close mock agent connection to clean up
	(<-mockAgentConn).Close()
	<-mockAgentDone
}

func TestAgentHandlerGPG(t *testing.T) {
	// Create a temporary Unix socket for the mock GPG agent
	tmpDir, err := os.MkdirTemp("", "dworm-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	socketPath := filepath.Join(tmpDir, "gpg-agent.sock")

	// Create a mock GPG agent
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("failed to create mock agent socket: %v", err)
	}
	defer listener.Close()

	// Start mock agent - just accepts and waits
	mockAgentConn := make(chan net.Conn, 1)
	mockAgentDone := make(chan struct{})
	go func() {
		defer close(mockAgentDone)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		mockAgentConn <- conn
		// Just wait for close
		buf := make([]byte, 1)
		conn.Read(buf) // Will return when closed
	}()

	// Create test harness
	h, err := testutil.NewTestHarness()
	if err != nil {
		t.Fatalf("failed to create harness: %v", err)
	}
	defer h.Close()

	// Create agent handler with GPG support
	logger := log.New(io.Discard, "", 0)
	handler := NewAgentHandler(h.HostMux, "", logger)
	handler.SetGPGSocketPath(socketPath)
	handler.Start()
	defer handler.Close()

	// Endpoint opens a stream with GPG agent type marker
	stream, err := h.EndpointMux.OpenStream()
	if err != nil {
		t.Fatalf("failed to open stream: %v", err)
	}
	defer stream.Close()

	// Send GPG agent type marker
	if _, err := stream.Write([]byte{protocol.StreamTypeGPG}); err != nil {
		t.Fatalf("failed to write stream type: %v", err)
	}

	// Read success response
	resp := make([]byte, 1)
	if _, err := io.ReadFull(stream, resp); err != nil {
		t.Fatalf("failed to read response: %v", err)
	}

	if resp[0] != 1 {
		t.Errorf("expected success (1), got %d", resp[0])
	}

	// Close mock agent connection to clean up
	(<-mockAgentConn).Close()
	<-mockAgentDone
}

func TestAgentHandlerUnknownType(t *testing.T) {
	// Create test harness
	h, err := testutil.NewTestHarness()
	if err != nil {
		t.Fatalf("failed to create harness: %v", err)
	}
	defer h.Close()

	// Create agent handler with no socket paths
	logger := log.New(io.Discard, "", 0)
	handler := NewAgentHandler(h.HostMux, "", logger)
	handler.Start()
	defer handler.Close()

	// Endpoint opens a stream with unknown type marker
	stream, err := h.EndpointMux.OpenStream()
	if err != nil {
		t.Fatalf("failed to open stream: %v", err)
	}
	defer stream.Close()

	// Send unknown stream type marker
	if _, err := stream.Write([]byte{0xFF}); err != nil {
		t.Fatalf("failed to write stream type: %v", err)
	}

	// Read failure response
	resp := make([]byte, 1)
	if _, err := io.ReadFull(stream, resp); err != nil {
		t.Fatalf("failed to read response: %v", err)
	}

	if resp[0] != 0 {
		t.Errorf("expected failure (0), got %d", resp[0])
	}
}

func TestAgentHandlerNoSocketConfigured(t *testing.T) {
	// Create test harness
	h, err := testutil.NewTestHarness()
	if err != nil {
		t.Fatalf("failed to create harness: %v", err)
	}
	defer h.Close()

	// Create agent handler with no SSH socket path
	logger := log.New(io.Discard, "", 0)
	handler := NewAgentHandler(h.HostMux, "", logger)
	handler.Start()
	defer handler.Close()

	// Endpoint tries SSH agent forwarding
	stream, err := h.EndpointMux.OpenStream()
	if err != nil {
		t.Fatalf("failed to open stream: %v", err)
	}
	defer stream.Close()

	// Send SSH agent type marker
	if _, err := stream.Write([]byte{protocol.StreamTypeAgent}); err != nil {
		t.Fatalf("failed to write stream type: %v", err)
	}

	// Should get failure response (socket not configured)
	resp := make([]byte, 1)
	if _, err := io.ReadFull(stream, resp); err != nil {
		t.Fatalf("failed to read response: %v", err)
	}

	if resp[0] != 0 {
		t.Errorf("expected failure (0), got %d", resp[0])
	}
}

func TestAgentHandlerGitCredentialProtocol(t *testing.T) {
	// Create test harness
	h, err := testutil.NewTestHarness()
	if err != nil {
		t.Fatalf("failed to create harness: %v", err)
	}
	defer h.Close()

	// Create agent handler
	logger := log.New(io.Discard, "", 0)
	handler := NewAgentHandler(h.HostMux, "", logger)
	handler.Start()
	defer handler.Close()

	// Endpoint opens a stream for git credentials
	stream, err := h.EndpointMux.OpenStream()
	if err != nil {
		t.Fatalf("failed to open stream: %v", err)
	}
	defer stream.Close()

	// Send git credential stream type marker
	if _, err := stream.Write([]byte{protocol.StreamTypeGitCred}); err != nil {
		t.Fatalf("failed to write stream type: %v", err)
	}

	// Send action (0x00 = fill)
	if _, err := stream.Write([]byte{0x00}); err != nil {
		t.Fatalf("failed to write action: %v", err)
	}

	// Send empty input length
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, 0)
	if _, err := stream.Write(lenBuf); err != nil {
		t.Fatalf("failed to write input length: %v", err)
	}

	// Read success acknowledgment
	resp := make([]byte, 1)
	if _, err := io.ReadFull(stream, resp); err != nil {
		t.Fatalf("failed to read response: %v", err)
	}

	if resp[0] != 1 {
		t.Errorf("expected success ack (1), got %d", resp[0])
	}

	// Read response length (git credential fill with empty input will likely fail,
	// but the protocol should still work)
	respLen := make([]byte, 4)
	if _, err := io.ReadFull(stream, respLen); err != nil {
		t.Fatalf("failed to read response length: %v", err)
	}

	// Just verify we got a valid response length (even if 0 due to no credentials)
	length := binary.BigEndian.Uint32(respLen)
	t.Logf("git credential response length: %d", length)
}

func TestNewAgentHandler(t *testing.T) {
	h, err := testutil.NewTestHarness()
	if err != nil {
		t.Fatalf("failed to create harness: %v", err)
	}
	defer h.Close()

	logger := log.New(io.Discard, "", 0)
	handler := NewAgentHandler(h.HostMux, "/tmp/test.sock", logger)

	if handler.mux != h.HostMux {
		t.Error("mux not set correctly")
	}
	if handler.sshSocketPath != "/tmp/test.sock" {
		t.Error("sshSocketPath not set correctly")
	}
	if handler.logger != logger {
		t.Error("logger not set correctly")
	}
	if handler.gpgSocketPath != "" {
		t.Error("gpgSocketPath should be empty initially")
	}

	handler.SetGPGSocketPath("/tmp/gpg.sock")
	if handler.gpgSocketPath != "/tmp/gpg.sock" {
		t.Error("SetGPGSocketPath didn't set path correctly")
	}
}
