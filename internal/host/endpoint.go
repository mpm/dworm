package host

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/mpm/dworm/internal/protocol"
)

// EndpointManager handles communication with the endpoint inside the container
type EndpointManager struct {
	containerID  string
	endpointPath string
	mux          *protocol.Mux
	cmd          *exec.Cmd
	logger       *log.Logger
}

// NewEndpointManager creates a new endpoint manager
func NewEndpointManager(containerID string) *EndpointManager {
	return &EndpointManager{
		containerID: containerID,
		logger:      log.New(protocol.NewCRWriter(os.Stderr), "[host] ", log.LstdFlags),
	}
}

// InjectAndStart copies the endpoint binary to the container and starts it
func (e *EndpointManager) InjectAndStart(endpointBinaryPath string) error {
	// Copy binary to container
	containerPath := "/tmp/dworm_endpoint"

	e.logger.Printf("Copying endpoint binary to container...")
	copyCmd := exec.Command("docker", "cp", endpointBinaryPath, fmt.Sprintf("%s:%s", e.containerID, containerPath))
	if output, err := copyCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to copy endpoint binary: %w\noutput: %s", err, string(output))
	}

	// Make executable
	chmodCmd := exec.Command("docker", "exec", e.containerID, "chmod", "+x", containerPath)
	if output, err := chmodCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to chmod endpoint binary: %w\noutput: %s", err, string(output))
	}

	e.endpointPath = containerPath

	// Start endpoint process
	e.logger.Printf("Starting endpoint process...")
	e.cmd = exec.Command("docker", "exec", "-i", e.containerID, containerPath)

	stdin, err := e.cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("failed to get stdin pipe: %w", err)
	}

	stdout, err := e.cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to get stdout pipe: %w", err)
	}

	// Forward stderr to our stderr
	e.cmd.Stderr = os.Stderr

	if err := e.cmd.Start(); err != nil {
		return fmt.Errorf("failed to start endpoint: %w", err)
	}

	// Create multiplexer
	rwc := &pipeReadWriteCloser{
		reader: stdout,
		writer: stdin,
	}

	e.mux, err = protocol.NewClientMux(rwc)
	if err != nil {
		e.cmd.Process.Kill()
		return fmt.Errorf("failed to create mux: %w", err)
	}

	e.logger.Printf("Endpoint started successfully")
	return nil
}

// SendInit sends the init message with environment variables and agent forwarding config
func (e *EndpointManager) SendInit(envVars map[string]string, agentForward bool, agentSocketPath string, gpgForward bool, gpgSocketPath string, gitConfigContent string, gitCredForward bool) error {
	return e.mux.SendControl(protocol.TypeInit, &protocol.InitMessage{
		EnvVars:          envVars,
		AgentForward:     agentForward,
		AgentSocketPath:  agentSocketPath,
		GPGForward:       gpgForward,
		GPGSocketPath:    gpgSocketPath,
		GitConfigContent: gitConfigContent,
		GitCredForward:   gitCredForward,
	})
}

// GetMux returns the underlying multiplexer for agent handling
func (e *EndpointManager) GetMux() *protocol.Mux {
	return e.mux
}

// RecvControl receives a control message
func (e *EndpointManager) RecvControl() (string, []byte, error) {
	return e.mux.RecvControl()
}

// OpenTunnelStream opens a new stream for tunneling to a port
func (e *EndpointManager) OpenTunnelStream(port int) (io.ReadWriteCloser, error) {
	stream, err := e.mux.OpenStream()
	if err != nil {
		return nil, err
	}

	// Send port as 4-byte header
	portBuf := []byte{
		byte(port >> 24),
		byte(port >> 16),
		byte(port >> 8),
		byte(port),
	}

	if _, err := stream.Write(portBuf); err != nil {
		stream.Close()
		return nil, fmt.Errorf("failed to send port: %w", err)
	}

	// Read response
	respBuf := make([]byte, 1)
	if _, err := io.ReadFull(stream, respBuf); err != nil {
		stream.Close()
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if respBuf[0] != 1 {
		stream.Close()
		return nil, fmt.Errorf("tunnel connection failed")
	}

	return stream, nil
}

// Close shuts down the endpoint connection
func (e *EndpointManager) Close() error {
	if e.mux != nil {
		e.mux.Close()
	}
	if e.cmd != nil && e.cmd.Process != nil {
		e.cmd.Process.Kill()
		e.cmd.Wait()
	}
	return nil
}

// GetEndpointBinaryPath returns the path to the endpoint binary
func GetEndpointBinaryPath() (string, error) {
	// First check next to the dworm binary
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}

	binDir := filepath.Dir(exe)
	endpointPath := filepath.Join(binDir, "dworm_endpoint")

	if _, err := os.Stat(endpointPath); err == nil {
		return endpointPath, nil
	}

	// Check in common locations
	paths := []string{
		"/usr/local/bin/dworm_endpoint",
		"/usr/bin/dworm_endpoint",
		"./bin/dworm_endpoint",
		"./dworm_endpoint",
	}

	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}

	return "", fmt.Errorf("dworm_endpoint binary not found")
}

// pipeReadWriteCloser wraps pipes as a ReadWriteCloser
type pipeReadWriteCloser struct {
	reader io.ReadCloser
	writer io.WriteCloser
}

func (p *pipeReadWriteCloser) Read(b []byte) (int, error) {
	return p.reader.Read(b)
}

func (p *pipeReadWriteCloser) Write(b []byte) (int, error) {
	return p.writer.Write(b)
}

func (p *pipeReadWriteCloser) Close() error {
	p.writer.Close()
	return p.reader.Close()
}
