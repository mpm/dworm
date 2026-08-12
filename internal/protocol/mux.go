package protocol

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"sync"

	"github.com/hashicorp/yamux"
)

// Mux wraps a yamux session and provides helpers for control messages and tunnels
type Mux struct {
	session     *yamux.Session
	controlConn net.Conn
	controlLock sync.Mutex
	reader      *bufio.Reader
}

// NewServerMux creates a multiplexer in server mode (endpoint side)
func NewServerMux(rwc io.ReadWriteCloser, logger *log.Logger) (*Mux, error) {
	config := newYamuxConfig(logger)

	session, err := yamux.Server(rwc, config)
	if err != nil {
		return nil, fmt.Errorf("failed to create yamux server session: %w", err)
	}

	// Accept the control stream (stream 0)
	controlConn, err := session.Accept()
	if err != nil {
		session.Close()
		return nil, fmt.Errorf("failed to accept control stream: %w", err)
	}

	return &Mux{
		session:     session,
		controlConn: controlConn,
		reader:      bufio.NewReader(controlConn),
	}, nil
}

// NewClientMux creates a multiplexer in client mode (host side)
func NewClientMux(rwc io.ReadWriteCloser, logger *log.Logger) (*Mux, error) {
	config := newYamuxConfig(logger)

	session, err := yamux.Client(rwc, config)
	if err != nil {
		return nil, fmt.Errorf("failed to create yamux client session: %w", err)
	}

	// Open the control stream (stream 0)
	controlConn, err := session.Open()
	if err != nil {
		session.Close()
		return nil, fmt.Errorf("failed to open control stream: %w", err)
	}

	return &Mux{
		session:     session,
		controlConn: controlConn,
		reader:      bufio.NewReader(controlConn),
	}, nil
}

func newYamuxConfig(logger *log.Logger) *yamux.Config {
	config := yamux.DefaultConfig()
	config.LogOutput = nil
	config.Logger = logger
	return config
}

// SendControl sends a control message over the control channel
func (m *Mux) SendControl(msgType string, data interface{}) error {
	encoded, err := EncodeMessage(msgType, data)
	if err != nil {
		return err
	}

	m.controlLock.Lock()
	defer m.controlLock.Unlock()

	// Write length prefix (4 bytes, big endian)
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(encoded)))

	if _, err := m.controlConn.Write(lenBuf); err != nil {
		return fmt.Errorf("failed to write message length: %w", err)
	}

	if _, err := m.controlConn.Write(encoded); err != nil {
		return fmt.Errorf("failed to write message data: %w", err)
	}

	return nil
}

// RecvControl receives a control message from the control channel
func (m *Mux) RecvControl() (string, []byte, error) {
	// Read length prefix
	lenBuf := make([]byte, 4)
	if _, err := io.ReadFull(m.reader, lenBuf); err != nil {
		return "", nil, fmt.Errorf("failed to read message length: %w", err)
	}

	length := binary.BigEndian.Uint32(lenBuf)
	if length > MaxControlMessageSize {
		return "", nil, fmt.Errorf("message too large: %d bytes", length)
	}

	// Read message data
	data := make([]byte, length)
	if _, err := io.ReadFull(m.reader, data); err != nil {
		return "", nil, fmt.Errorf("failed to read message data: %w", err)
	}

	msgType, rawData, err := DecodeMessage(data)
	if err != nil {
		return "", nil, err
	}

	return msgType, rawData, nil
}

// OpenStream opens a new stream for tunneling
func (m *Mux) OpenStream() (net.Conn, error) {
	return m.session.Open()
}

// AcceptStream accepts a new stream for tunneling
func (m *Mux) AcceptStream() (net.Conn, error) {
	return m.session.Accept()
}

// Close closes the multiplexer and underlying session
func (m *Mux) Close() error {
	m.controlConn.Close()
	return m.session.Close()
}

// IsClosed returns true if the session is closed
func (m *Mux) IsClosed() bool {
	return m.session.IsClosed()
}
