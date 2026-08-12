package endpoint

import (
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/mpm/dworm/internal/protocol"
)

// Server is the endpoint server running inside the container
type Server struct {
	mux              *protocol.Mux
	envVars          map[string]string
	portStateMu      sync.RWMutex
	currentPorts     []protocol.PortInfo
	portAddresses    map[int]string // port -> best bind address for connecting
	logger           *log.Logger
	agentForwarder   *AgentForwarder
	gpgForwarder     *GPGForwarder
	gitCredForwarder *GitCredForwarder
}

// NewServer creates a new endpoint server
func NewServer() *Server {
	return &Server{
		logger:        log.New(protocol.NewCRWriter(os.Stderr), "[endpoint] ", log.LstdFlags),
		portAddresses: make(map[int]string),
	}
}

// Run starts the endpoint server
func (s *Server) Run() error {
	s.logger.Println("Starting endpoint server")

	// Create multiplexer over stdin/stdout
	rwc := &stdioReadWriteCloser{
		reader: os.Stdin,
		writer: os.Stdout,
	}

	var err error
	s.mux, err = protocol.NewServerMux(rwc)
	if err != nil {
		return fmt.Errorf("failed to create mux: %w", err)
	}
	defer s.cleanup()

	// Wait for init message
	if err := s.waitForInit(); err != nil {
		return fmt.Errorf("init failed: %w", err)
	}

	// Start port scanner
	go s.runPortScanner()

	// Start accepting tunnel streams
	go s.acceptTunnelStreams()

	// Handle control messages
	return s.handleControlMessages()
}

func (s *Server) waitForInit() error {
	msgType, data, err := s.mux.RecvControl()
	if err != nil {
		return err
	}

	if msgType != protocol.TypeInit {
		return fmt.Errorf("expected init message, got %s", msgType)
	}

	initMsg, err := protocol.DecodeInit(data)
	if err != nil {
		return err
	}

	s.envVars = initMsg.EnvVars

	// Set environment variables
	if err := SetEnvironment(s.envVars); err != nil {
		s.logger.Printf("Warning: failed to set some env vars: %v", err)
	}

	// Start SSH agent forwarder if enabled
	if initMsg.AgentForward && initMsg.AgentSocketPath != "" {
		s.agentForwarder = NewAgentForwarder(initMsg.AgentSocketPath, s.mux, s.logger)
		if err := s.agentForwarder.Start(); err != nil {
			s.logger.Printf("Warning: failed to start agent forwarder: %v", err)
		}
	}

	// Import GPG public keys if provided (before starting GPG forwarder)
	if initMsg.GPGPublicKeys != "" {
		importCmd := exec.Command("gpg", "--import", "--batch")
		importCmd.Stdin = strings.NewReader(initMsg.GPGPublicKeys)
		if output, err := importCmd.CombinedOutput(); err != nil {
			s.logger.Printf("Warning: failed to import GPG public keys: %v (output: %s)", err, string(output))
		} else {
			s.logger.Printf("GPG public keys imported")
		}
	}

	// Start GPG agent forwarder if enabled
	if initMsg.GPGForward && initMsg.GPGSocketPath != "" {
		s.gpgForwarder = NewGPGForwarder(initMsg.GPGSocketPath, s.mux, s.logger)
		if err := s.gpgForwarder.Start(); err != nil {
			s.logger.Printf("Warning: failed to start GPG agent forwarder: %v", err)
		}
	}

	// Write git config if provided
	if initMsg.GitConfigContent != "" {
		if err := WriteGitConfig(initMsg.GitConfigContent); err != nil {
			s.logger.Printf("Warning: failed to write git config: %v", err)
		} else {
			s.logger.Printf("Git config written to ~/.gitconfig")
		}
	}

	// Start git credential forwarder if enabled
	if initMsg.GitCredForward {
		socketPath := protocol.GitCredentialSocket
		helperPath := protocol.GitCredentialHelperPath

		s.gitCredForwarder = NewGitCredForwarder(socketPath, s.mux, s.logger)
		if err := s.gitCredForwarder.Start(); err != nil {
			s.logger.Printf("Warning: failed to start git credential forwarder: %v", err)
		} else {
			// Create the credential helper script
			if err := CreateCredentialHelper(helperPath); err != nil {
				s.logger.Printf("Warning: failed to create credential helper script: %v", err)
			} else {
				// Configure git to use the helper
				if err := ConfigureCredentialHelper(helperPath); err != nil {
					s.logger.Printf("Warning: failed to configure git credential helper: %v", err)
				} else {
					s.logger.Printf("Git credential forwarding configured")
				}
			}
		}
	}

	s.logger.Printf("Initialized with %d env vars", len(s.envVars))
	return nil
}

func (s *Server) runPortScanner() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	// Initial scan
	s.scanAndReport()

	for range ticker.C {
		if s.mux.IsClosed() {
			return
		}
		s.scanAndReport()
	}
}

func (s *Server) scanAndReport() {
	ports, err := ScanListeningPorts()
	if err != nil {
		s.logger.Printf("Port scan error: %v", err)
		return
	}

	previousPorts, changed := s.updatePortState(ports)
	if changed {
		added, removed := DiffPorts(previousPorts, ports)
		if len(added) > 0 {
			s.logger.Printf("New ports detected: %v", added)
		}
		if len(removed) > 0 {
			s.logger.Printf("Ports closed: %v", removed)
		}

		// Send port update
		if err := s.mux.SendControl(protocol.TypePortUpdate, &protocol.PortUpdateMessage{
			Ports: ports,
		}); err != nil {
			s.logger.Printf("Failed to send port update: %v", err)
		}
	}
}

func (s *Server) updatePortState(ports []protocol.PortInfo) ([]protocol.PortInfo, bool) {
	addresses := make(map[int]string)
	for _, p := range ports {
		existing, ok := addresses[p.Port]
		if !ok {
			addresses[p.Port] = p.Address
		} else {
			addresses[p.Port] = preferAddress(existing, p.Address)
		}
	}

	s.portStateMu.Lock()
	defer s.portStateMu.Unlock()
	if reflect.DeepEqual(ports, s.currentPorts) {
		return nil, false
	}

	previousPorts := s.currentPorts
	s.currentPorts = ports
	s.portAddresses = addresses
	return previousPorts, true
}

func (s *Server) portAddress(port int) (string, bool) {
	s.portStateMu.RLock()
	defer s.portStateMu.RUnlock()
	address, ok := s.portAddresses[port]
	return address, ok
}

// preferAddress returns the address that's more likely to be connectable
// Priority: 0.0.0.0/:: (all interfaces) > specific IPs > localhost
func preferAddress(a, b string) string {
	priority := func(addr string) int {
		switch addr {
		case "0.0.0.0", "::":
			return 3 // Highest - listens on all interfaces
		case "127.0.0.1", "::1":
			return 1 // Lowest - only localhost
		default:
			return 2 // Middle - specific interface
		}
	}
	if priority(a) >= priority(b) {
		return a
	}
	return b
}

func (s *Server) acceptTunnelStreams() {
	for {
		stream, err := s.mux.AcceptStream()
		if err != nil {
			if s.mux.IsClosed() {
				return
			}
			s.logger.Printf("Failed to accept stream: %v", err)
			continue
		}

		go s.handleTunnelStream(stream)
	}
}

func (s *Server) handleTunnelStream(stream net.Conn) {
	defer stream.Close()

	// Read tunnel request header (4 bytes for port)
	portBuf := make([]byte, 4)
	if _, err := io.ReadFull(stream, portBuf); err != nil {
		s.logger.Printf("Failed to read tunnel port: %v", err)
		return
	}

	port := int(portBuf[0])<<24 | int(portBuf[1])<<16 | int(portBuf[2])<<8 | int(portBuf[3])

	// Look up the bind address for this port
	bindAddr, ok := s.portAddress(port)
	if !ok {
		// Fallback to localhost if port not in our map (shouldn't happen normally)
		bindAddr = "127.0.0.1"
	}

	// Format the dial address (IPv6 addresses need brackets)
	var dialAddr string
	if strings.Contains(bindAddr, ":") {
		// IPv6 address
		dialAddr = fmt.Sprintf("[%s]:%d", bindAddr, port)
	} else {
		dialAddr = fmt.Sprintf("%s:%d", bindAddr, port)
	}

	// For addresses like 0.0.0.0 or ::, connect to localhost instead
	// (they listen on all interfaces, but we connect via localhost)
	if bindAddr == "0.0.0.0" {
		dialAddr = fmt.Sprintf("127.0.0.1:%d", port)
	} else if bindAddr == "::" {
		dialAddr = fmt.Sprintf("[::1]:%d", port)
	}

	// Connect to local port
	localConn, err := net.Dial("tcp", dialAddr)
	if err != nil {
		s.logger.Printf("Failed to connect to local port %d (%s): %v", port, dialAddr, err)
		// Send error response
		stream.Write([]byte{0}) // 0 = failure
		return
	}
	defer localConn.Close()

	// Send success response
	stream.Write([]byte{1}) // 1 = success

	// Proxy data bidirectionally
	if err := protocol.BiProxy(stream, localConn); err != nil {
		s.logger.Printf("Tunnel for port %d closed with error: %v", port, err)
	}
}

func (s *Server) handleControlMessages() error {
	for {
		msgType, data, err := s.mux.RecvControl()
		if err != nil {
			if s.mux.IsClosed() {
				return nil
			}
			return fmt.Errorf("control message error: %w", err)
		}

		switch msgType {
		case protocol.TypePing:
			if err := s.mux.SendControl(protocol.TypePong, nil); err != nil {
				s.logger.Printf("Failed to send pong: %v", err)
			}
		default:
			s.logger.Printf("Unknown message type: %s (data: %s)", msgType, string(data))
		}
	}
}

func (s *Server) cleanup() {
	if s.agentForwarder != nil {
		s.agentForwarder.Close()
	}
	if s.gpgForwarder != nil {
		s.gpgForwarder.Close()
	}
	if s.gitCredForwarder != nil {
		s.gitCredForwarder.Close()
	}
	if s.mux != nil {
		s.mux.Close()
	}
}

// stdioReadWriteCloser wraps stdin/stdout as a ReadWriteCloser
type stdioReadWriteCloser struct {
	reader io.Reader
	writer io.Writer
}

func (s *stdioReadWriteCloser) Read(p []byte) (int, error) {
	return s.reader.Read(p)
}

func (s *stdioReadWriteCloser) Write(p []byte) (int, error) {
	return s.writer.Write(p)
}

func (s *stdioReadWriteCloser) Close() error {
	return nil
}
