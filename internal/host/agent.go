package host

import (
	"bytes"
	"encoding/binary"
	"io"
	"log"
	"net"
	"os/exec"
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
	case protocol.StreamTypeGitCred:
		a.handleGitCredential(stream)
		return
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

// handleGitCredential handles a git credential request from the endpoint
func (a *AgentHandler) handleGitCredential(stream net.Conn) {
	// Read action byte (0=get/fill, 1=store/approve, 2=erase/reject)
	actionBuf := make([]byte, 1)
	if _, err := io.ReadFull(stream, actionBuf); err != nil {
		a.logger.Printf("Failed to read git credential action: %v", err)
		stream.Write([]byte{0}) // failure
		return
	}

	var action string
	switch actionBuf[0] {
	case 0x00:
		action = "fill"
	case 0x01:
		action = "approve"
	case 0x02:
		action = "reject"
	default:
		a.logger.Printf("Unknown git credential action: %d", actionBuf[0])
		stream.Write([]byte{0}) // failure
		return
	}

	// Read input length
	lenBuf := make([]byte, 4)
	if _, err := io.ReadFull(stream, lenBuf); err != nil {
		a.logger.Printf("Failed to read git credential input length: %v", err)
		stream.Write([]byte{0}) // failure
		return
	}
	inputLen := binary.BigEndian.Uint32(lenBuf)

	// Read input
	input := make([]byte, inputLen)
	if inputLen > 0 {
		if _, err := io.ReadFull(stream, input); err != nil {
			a.logger.Printf("Failed to read git credential input: %v", err)
			stream.Write([]byte{0}) // failure
			return
		}
	}

	// Send success acknowledgment
	stream.Write([]byte{1})

	// Execute git credential on host
	cmd := exec.Command("git", "credential", action)
	cmd.Stdin = bytes.NewReader(input)
	output, err := cmd.Output()

	// Send response length and data
	respLen := make([]byte, 4)
	if err != nil {
		a.logger.Printf("Git credential %s failed: %v", action, err)
		binary.BigEndian.PutUint32(respLen, 0)
		stream.Write(respLen)
	} else {
		binary.BigEndian.PutUint32(respLen, uint32(len(output)))
		stream.Write(respLen)
		if len(output) > 0 {
			stream.Write(output)
		}
	}
}
