package host

import (
	"io"
	"log"
	"net"
	"sync"

	"github.com/mpm/dworm/internal/protocol"
)

// AgentHandler accepts agent forwarding streams from the endpoint (SSH and GPG)
type AgentHandler struct {
	mux           *protocol.Mux
	sshSocketPath string
	gpgSocketPath string
	logger        *log.Logger
	closed        bool
	closeMu       sync.Mutex
}

// NewAgentHandler creates a new agent handler for SSH agent forwarding
func NewAgentHandler(mux *protocol.Mux, sshSocketPath string, logger *log.Logger) *AgentHandler {
	return &AgentHandler{
		mux:           mux,
		sshSocketPath: sshSocketPath,
		logger:        logger,
	}
}

// SetGPGSocketPath enables GPG agent forwarding with the given socket path
func (a *AgentHandler) SetGPGSocketPath(gpgSocketPath string) {
	a.gpgSocketPath = gpgSocketPath
}

// Start begins accepting agent streams from the endpoint
func (a *AgentHandler) Start() {
	go a.acceptLoop()
}

func (a *AgentHandler) acceptLoop() {
	for {
		stream, err := a.mux.AcceptStream()
		if err != nil {
			a.closeMu.Lock()
			closed := a.closed
			a.closeMu.Unlock()
			if closed || a.mux.IsClosed() {
				return
			}
			a.logger.Printf("Agent stream accept error: %v", err)
			continue
		}

		go a.handleStream(stream)
	}
}

func (a *AgentHandler) handleStream(stream net.Conn) {
	defer stream.Close()

	// Read stream type marker (1 byte)
	typeBuf := make([]byte, 1)
	if _, err := io.ReadFull(stream, typeBuf); err != nil {
		a.logger.Printf("Failed to read stream type: %v", err)
		return
	}

	var socketPath string
	var agentType string

	switch typeBuf[0] {
	case protocol.StreamTypeAgent:
		socketPath = a.sshSocketPath
		agentType = "SSH"
	case protocol.StreamTypeGPG:
		socketPath = a.gpgSocketPath
		agentType = "GPG"
	default:
		a.logger.Printf("Unknown stream type: %d", typeBuf[0])
		stream.Write([]byte{0}) // failure
		return
	}

	if socketPath == "" {
		a.logger.Printf("%s agent forwarding not enabled", agentType)
		stream.Write([]byte{0}) // failure
		return
	}

	// Connect to local agent socket
	agentConn, err := net.Dial("unix", socketPath)
	if err != nil {
		a.logger.Printf("Failed to connect to %s agent: %v", agentType, err)
		stream.Write([]byte{0}) // failure
		return
	}
	defer agentConn.Close()

	// Send success response
	stream.Write([]byte{1})

	// Proxy data bidirectionally
	done := make(chan struct{}, 2)

	go func() {
		io.Copy(agentConn, stream)
		done <- struct{}{}
	}()

	go func() {
		io.Copy(stream, agentConn)
		done <- struct{}{}
	}()

	<-done
}

// Close stops accepting agent streams
func (a *AgentHandler) Close() {
	a.closeMu.Lock()
	a.closed = true
	a.closeMu.Unlock()
}
