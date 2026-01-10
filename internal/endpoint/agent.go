package endpoint

import (
	"io"
	"log"
	"net"
	"os"
	"sync"

	"github.com/mpm/dworm/internal/protocol"
)

// AgentForwarder listens on a Unix socket and forwards connections to the host's SSH agent
type AgentForwarder struct {
	socketPath string
	listener   net.Listener
	mux        *protocol.Mux
	logger     *log.Logger
	closed     bool
	closeMu    sync.Mutex
}

// NewAgentForwarder creates a new SSH agent forwarder
func NewAgentForwarder(socketPath string, mux *protocol.Mux, logger *log.Logger) *AgentForwarder {
	return &AgentForwarder{
		socketPath: socketPath,
		mux:        mux,
		logger:     logger,
	}
}

// Start begins listening for agent connections
func (a *AgentForwarder) Start() error {
	// Remove existing socket if present
	os.Remove(a.socketPath)

	listener, err := net.Listen("unix", a.socketPath)
	if err != nil {
		return err
	}
	a.listener = listener

	// Make socket accessible
	if err := os.Chmod(a.socketPath, 0600); err != nil {
		a.logger.Printf("Warning: failed to chmod agent socket: %v", err)
	}

	a.logger.Printf("SSH agent forwarder listening on %s", a.socketPath)

	go a.acceptLoop()
	return nil
}

func (a *AgentForwarder) acceptLoop() {
	for {
		conn, err := a.listener.Accept()
		if err != nil {
			a.closeMu.Lock()
			closed := a.closed
			a.closeMu.Unlock()
			if closed {
				return
			}
			a.logger.Printf("Agent accept error: %v", err)
			continue
		}

		go a.handleConnection(conn)
	}
}

func (a *AgentForwarder) handleConnection(conn net.Conn) {
	defer conn.Close()

	// Open stream to host
	stream, err := a.mux.OpenStream()
	if err != nil {
		a.logger.Printf("Failed to open agent stream: %v", err)
		return
	}
	defer stream.Close()

	// Send stream type marker
	if _, err := stream.Write([]byte{protocol.StreamTypeAgent}); err != nil {
		a.logger.Printf("Failed to send agent stream marker: %v", err)
		return
	}

	// Read response (1 byte: 1=success, 0=failure)
	resp := make([]byte, 1)
	if _, err := io.ReadFull(stream, resp); err != nil {
		a.logger.Printf("Failed to read agent response: %v", err)
		return
	}

	if resp[0] != 1 {
		a.logger.Printf("Host rejected agent connection")
		return
	}

	// Proxy data bidirectionally
	done := make(chan struct{}, 2)

	go func() {
		io.Copy(stream, conn)
		done <- struct{}{}
	}()

	go func() {
		io.Copy(conn, stream)
		done <- struct{}{}
	}()

	<-done
}

// Close stops the agent forwarder
func (a *AgentForwarder) Close() error {
	a.closeMu.Lock()
	a.closed = true
	a.closeMu.Unlock()

	if a.listener != nil {
		a.listener.Close()
	}
	os.Remove(a.socketPath)
	return nil
}

// SocketPath returns the path to the agent socket
func (a *AgentForwarder) SocketPath() string {
	return a.socketPath
}
