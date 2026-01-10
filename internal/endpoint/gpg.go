package endpoint

import (
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/mpm/dworm/internal/protocol"
)

// GPGForwarder listens on a Unix socket and forwards connections to the host's GPG agent
type GPGForwarder struct {
	socketPath string
	listener   net.Listener
	mux        *protocol.Mux
	logger     *log.Logger
	closed     bool
	closeMu    sync.Mutex
}

// NewGPGForwarder creates a new GPG agent forwarder
func NewGPGForwarder(socketPath string, mux *protocol.Mux, logger *log.Logger) *GPGForwarder {
	return &GPGForwarder{
		socketPath: socketPath,
		mux:        mux,
		logger:     logger,
	}
}

// Start begins listening for GPG agent connections
func (g *GPGForwarder) Start() error {
	// Determine the correct socket path using gpgconf
	// This is where GPG expects to find its agent socket
	socketPath, err := getGPGSocketPath()
	if err != nil {
		g.logger.Printf("Warning: could not determine GPG socket path, using fallback: %v", err)
		socketPath = g.socketPath // Use the path from init message as fallback
	} else {
		g.socketPath = socketPath
	}

	// Kill any existing gpg-agent to release the socket
	// This is safe in a devcontainer since we're replacing it with forwarding
	if err := exec.Command("gpgconf", "--kill", "gpg-agent").Run(); err != nil {
		g.logger.Printf("Note: could not kill existing gpg-agent: %v", err)
	}

	// Ensure the directory exists
	socketDir := filepath.Dir(g.socketPath)
	if err := os.MkdirAll(socketDir, 0700); err != nil {
		return err
	}

	// Remove existing socket if present
	os.Remove(g.socketPath)

	listener, err := net.Listen("unix", g.socketPath)
	if err != nil {
		return err
	}
	g.listener = listener

	// Make socket accessible
	if err := os.Chmod(g.socketPath, 0600); err != nil {
		g.logger.Printf("Warning: failed to chmod GPG agent socket: %v", err)
	}

	g.logger.Printf("GPG agent forwarder listening on %s", g.socketPath)

	go g.acceptLoop()
	return nil
}

// getGPGSocketPath returns the path where GPG expects its agent socket
func getGPGSocketPath() (string, error) {
	cmd := exec.Command("gpgconf", "--list-dirs", "agent-socket")
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

func (g *GPGForwarder) acceptLoop() {
	for {
		conn, err := g.listener.Accept()
		if err != nil {
			g.closeMu.Lock()
			closed := g.closed
			g.closeMu.Unlock()
			if closed {
				return
			}
			g.logger.Printf("GPG agent accept error: %v", err)
			continue
		}

		go g.handleConnection(conn)
	}
}

func (g *GPGForwarder) handleConnection(conn net.Conn) {
	defer conn.Close()

	// Open stream to host
	stream, err := g.mux.OpenStream()
	if err != nil {
		g.logger.Printf("Failed to open GPG agent stream: %v", err)
		return
	}
	defer stream.Close()

	// Send stream type marker for GPG
	if _, err := stream.Write([]byte{protocol.StreamTypeGPG}); err != nil {
		g.logger.Printf("Failed to send GPG agent stream marker: %v", err)
		return
	}

	// Read response (1 byte: 1=success, 0=failure)
	resp := make([]byte, 1)
	if _, err := io.ReadFull(stream, resp); err != nil {
		g.logger.Printf("Failed to read GPG agent response: %v", err)
		return
	}

	if resp[0] != 1 {
		g.logger.Printf("Host rejected GPG agent connection")
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

// Close stops the GPG agent forwarder
func (g *GPGForwarder) Close() error {
	g.closeMu.Lock()
	g.closed = true
	g.closeMu.Unlock()

	if g.listener != nil {
		g.listener.Close()
	}
	os.Remove(g.socketPath)
	return nil
}

// SocketPath returns the path to the GPG agent socket
func (g *GPGForwarder) SocketPath() string {
	return g.socketPath
}
