package endpoint

import (
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"

	"github.com/mpm/dworm/internal/protocol"
)

const (
	GitCredActionGet   byte = 0x00
	GitCredActionStore byte = 0x01
	GitCredActionErase byte = 0x02
)

// GitCredForwarder listens on a Unix socket and forwards git credential requests to the host
type GitCredForwarder struct {
	socketPath string
	listener   net.Listener
	mux        *protocol.Mux
	logger     *log.Logger
	closed     bool
	closeMu    sync.Mutex
}

// NewGitCredForwarder creates a new git credential forwarder
func NewGitCredForwarder(socketPath string, mux *protocol.Mux, logger *log.Logger) *GitCredForwarder {
	return &GitCredForwarder{
		socketPath: socketPath,
		mux:        mux,
		logger:     logger,
	}
}

// Start begins listening for credential requests
func (g *GitCredForwarder) Start() error {
	// Remove existing socket if present
	os.Remove(g.socketPath)

	listener, err := net.Listen("unix", g.socketPath)
	if err != nil {
		return err
	}
	g.listener = listener

	// Make socket accessible
	if err := os.Chmod(g.socketPath, 0600); err != nil {
		g.logger.Printf("Warning: failed to chmod git credential socket: %v", err)
	}

	g.logger.Printf("Git credential forwarder listening on %s", g.socketPath)

	go g.acceptLoop()
	return nil
}

func (g *GitCredForwarder) acceptLoop() {
	for {
		conn, err := g.listener.Accept()
		if err != nil {
			g.closeMu.Lock()
			closed := g.closed
			g.closeMu.Unlock()
			if closed {
				return
			}
			g.logger.Printf("Git credential accept error: %v", err)
			continue
		}

		go g.handleConnection(conn)
	}
}

func (g *GitCredForwarder) handleConnection(conn net.Conn) {
	defer conn.Close()

	// Protocol: first line is action, rest is input
	// Read action byte first
	actionBuf := make([]byte, 1)
	if _, err := io.ReadFull(conn, actionBuf); err != nil {
		g.logger.Printf("Failed to read git credential action: %v", err)
		return
	}

	action := actionBuf[0]

	// Read rest as input (read until EOF or a reasonable limit)
	input, err := io.ReadAll(io.LimitReader(conn, protocol.MaxGitCredentialInput))
	if err != nil {
		g.logger.Printf("Failed to read git credential input: %v", err)
		return
	}

	// Open stream to host
	stream, err := g.mux.OpenStream()
	if err != nil {
		g.logger.Printf("Failed to open git credential stream: %v", err)
		return
	}
	defer stream.Close()

	// Send stream type marker
	if _, err := stream.Write([]byte{protocol.StreamTypeGitCred}); err != nil {
		g.logger.Printf("Failed to send git credential stream marker: %v", err)
		return
	}

	// Send action
	if _, err := stream.Write([]byte{action}); err != nil {
		g.logger.Printf("Failed to send git credential action: %v", err)
		return
	}

	// Send input length
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(input)))
	if _, err := stream.Write(lenBuf); err != nil {
		g.logger.Printf("Failed to send git credential input length: %v", err)
		return
	}

	// Send input
	if len(input) > 0 {
		if _, err := stream.Write(input); err != nil {
			g.logger.Printf("Failed to send git credential input: %v", err)
			return
		}
	}

	// Read response (1 byte: success/failure)
	resp := make([]byte, 1)
	if _, err := io.ReadFull(stream, resp); err != nil {
		g.logger.Printf("Failed to read git credential response: %v", err)
		return
	}

	if resp[0] != 1 {
		g.logger.Printf("Host rejected git credential request")
		return
	}

	// Read response length
	if _, err := io.ReadFull(stream, lenBuf); err != nil {
		g.logger.Printf("Failed to read git credential response length: %v", err)
		return
	}
	respLen := binary.BigEndian.Uint32(lenBuf)

	// Read and forward response
	if respLen > 0 {
		response := make([]byte, respLen)
		if _, err := io.ReadFull(stream, response); err != nil {
			g.logger.Printf("Failed to read git credential response: %v", err)
			return
		}
		conn.Write(response)
	}
}

// Close stops the git credential forwarder
func (g *GitCredForwarder) Close() error {
	g.closeMu.Lock()
	g.closed = true
	g.closeMu.Unlock()

	if g.listener != nil {
		g.listener.Close()
	}
	os.Remove(g.socketPath)
	return nil
}

// RunCredentialHelper is called when the endpoint binary is invoked with --credential-helper flag
// It connects to the socket and forwards the credential request
func RunCredentialHelper(socketPath string, action string) error {
	// Map action string to byte
	var actionByte byte
	switch action {
	case "get":
		actionByte = GitCredActionGet
	case "store":
		actionByte = GitCredActionStore
	case "erase":
		actionByte = GitCredActionErase
	default:
		return fmt.Errorf("unknown credential action: %s", action)
	}

	// Connect to socket
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return fmt.Errorf("failed to connect to credential socket: %w", err)
	}
	defer conn.Close()

	// Send action
	if _, err := conn.Write([]byte{actionByte}); err != nil {
		return fmt.Errorf("failed to send action: %w", err)
	}

	// Read stdin and send to socket
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("failed to read stdin: %w", err)
	}
	if _, err := conn.Write(input); err != nil {
		return fmt.Errorf("failed to send input: %w", err)
	}

	// Signal end of input by closing write side
	if unixConn, ok := conn.(*net.UnixConn); ok {
		unixConn.CloseWrite()
	}

	// Read response and write to stdout
	response, err := io.ReadAll(conn)
	if err != nil {
		return fmt.Errorf("failed to read response: %w", err)
	}
	os.Stdout.Write(response)

	return nil
}
