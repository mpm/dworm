package endpoint

import (
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"reflect"
	"time"

	"github.com/mpm/dworm/internal/protocol"
)

// Server is the endpoint server running inside the container
type Server struct {
	mux          *protocol.Mux
	envVars      map[string]string
	currentPorts []int
	logger       *log.Logger
}

// NewServer creates a new endpoint server
func NewServer() *Server {
	return &Server{
		logger: log.New(os.Stderr, "[endpoint] ", log.LstdFlags),
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
	defer s.mux.Close()

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

	// Only report if changed
	if !reflect.DeepEqual(ports, s.currentPorts) {
		added, removed := DiffPorts(s.currentPorts, ports)
		if len(added) > 0 {
			s.logger.Printf("New ports detected: %v", added)
		}
		if len(removed) > 0 {
			s.logger.Printf("Ports closed: %v", removed)
		}

		s.currentPorts = ports

		// Send port update
		if err := s.mux.SendControl(protocol.TypePortUpdate, &protocol.PortUpdateMessage{
			Ports: ports,
		}); err != nil {
			s.logger.Printf("Failed to send port update: %v", err)
		}
	}
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

	// Connect to local port
	localConn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		s.logger.Printf("Failed to connect to local port %d: %v", port, err)
		// Send error response
		stream.Write([]byte{0}) // 0 = failure
		return
	}
	defer localConn.Close()

	// Send success response
	stream.Write([]byte{1}) // 1 = success

	// Proxy data bidirectionally
	done := make(chan struct{}, 2)

	go func() {
		io.Copy(localConn, stream)
		done <- struct{}{}
	}()

	go func() {
		io.Copy(stream, localConn)
		done <- struct{}{}
	}()

	<-done
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
