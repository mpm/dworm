package host

import (
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/mpm/dworm/internal/host/endpointbundle"
	"github.com/mpm/dworm/internal/protocol"
)

// EndpointManager handles communication with the endpoint inside the container
type EndpointManager struct {
	containerID  string
	endpointPath string
	mux          *protocol.Mux
	cmd          *exec.Cmd
	logger       *log.Logger
	stderrWriter io.Writer
	setupTimeout time.Duration
}

// NewEndpointManager creates a new endpoint manager.
// If logWriter is non-nil, it is used for both the host logger and endpoint stderr.
// Otherwise, logs go to os.Stderr via CRWriter.
func NewEndpointManager(containerID string, logWriter io.Writer) *EndpointManager {
	stderr := io.Writer(protocol.NewCRWriter(os.Stderr))
	if logWriter != nil {
		stderr = logWriter
	}
	return &EndpointManager{
		containerID:  containerID,
		logger:       log.New(stderr, "[host] ", log.LstdFlags),
		stderrWriter: stderr,
		setupTimeout: protocol.TunnelSetupTimeout,
	}
}

// InjectAndStart copies the endpoint binary to the container and starts it
func (e *EndpointManager) InjectAndStart() error {
	// Inspect the immutable image ID used by this container, not a mutable tag
	// or the Docker daemon's own architecture (which may run emulated images).
	image, err := exec.Command("docker", "inspect", "--type", "container", "--format", "{{.Image}}", e.containerID).Output()
	if err != nil {
		return fmt.Errorf("inspect container image: %w", err)
	}
	platform, err := exec.Command("docker", "image", "inspect", "--format", "{{.Os}}/{{.Architecture}}", strings.TrimSpace(string(image))).Output()
	if err != nil {
		return fmt.Errorf("inspect container image platform: %w", err)
	}
	payload, err := endpointbundle.Payload(string(platform))
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "dworm-endpoint-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	endpointBinaryPath := filepath.Join(dir, "dworm_endpoint")
	if err := os.WriteFile(endpointBinaryPath, payload, 0700); err != nil {
		return err
	}
	// Copy binary to container
	containerPath := "/tmp/dworm_endpoint"
	// Rename a unique staged file atomically: existing processes keep their inode.
	stage := containerPath + "." + filepath.Base(dir)
	defer exec.Command("docker", "exec", e.containerID, "rm", "-f", stage).Run()

	e.logger.Printf("Copying endpoint binary to container...")
	copyCmd := exec.Command("docker", "cp", endpointBinaryPath, fmt.Sprintf("%s:%s", e.containerID, stage))
	if output, err := copyCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to copy endpoint binary: %w\noutput: %s", err, string(output))
	}

	// Make executable
	chmodCmd := exec.Command("docker", "exec", e.containerID, "chmod", "755", stage)
	if output, err := chmodCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to chmod endpoint binary: %w\noutput: %s", err, string(output))
	}

	if output, err := exec.Command("docker", "exec", e.containerID, "mv", "-f", stage, containerPath).CombinedOutput(); err != nil {
		return fmt.Errorf("failed to publish endpoint: %w: %s", err, output)
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

	// Forward stderr to configured writer
	e.cmd.Stderr = e.stderrWriter

	if err := e.cmd.Start(); err != nil {
		return fmt.Errorf("failed to start endpoint: %w", err)
	}

	// Create multiplexer
	rwc := &pipeReadWriteCloser{
		reader: stdout,
		writer: stdin,
	}

	e.mux, err = protocol.NewClientMux(rwc, e.logger)
	if err != nil {
		e.cmd.Process.Kill()
		return fmt.Errorf("failed to create mux: %w", err)
	}

	e.logger.Printf("Endpoint started successfully")
	return nil
}

// SendInit sends the init message with environment variables and agent forwarding config
func (e *EndpointManager) SendInit(envVars map[string]string, agentForward bool, agentSocketPath string, gpgForward bool, gpgSocketPath string, gitConfigContent string, gitCredForward bool, gpgPublicKeys string) error {
	return e.mux.SendControl(protocol.TypeInit, &protocol.InitMessage{
		EnvVars:          envVars,
		AgentForward:     agentForward,
		AgentSocketPath:  agentSocketPath,
		GPGForward:       gpgForward,
		GPGSocketPath:    gpgSocketPath,
		GitConfigContent: gitConfigContent,
		GitCredForward:   gitCredForward,
		GPGPublicKeys:    gpgPublicKeys,
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

// WaitEnvironmentReady is called before starting the general control reader.
func (e *EndpointManager) WaitEnvironmentReady() error {
	result := make(chan error, 1)
	go func() {
		kind, _, err := e.RecvControl()
		if err == nil && kind != protocol.TypeEnvironmentReady {
			err = fmt.Errorf("expected environment acknowledgement, got %s", kind)
		}
		result <- err
	}()
	select {
	case err := <-result:
		return err
	case <-time.After(30 * time.Second):
		e.mux.Close()
		return fmt.Errorf("timed out waiting for environment initialization")
	}
}

// OpenTunnelStream opens a new stream for tunneling to a port
func (e *EndpointManager) OpenTunnelStream(port int) (net.Conn, error) {
	stream, err := e.mux.OpenStream()
	if err != nil {
		return nil, err
	}
	setupTimeout := e.setupTimeout
	if setupTimeout == 0 {
		setupTimeout = protocol.TunnelSetupTimeout
	}
	if err := stream.SetDeadline(time.Now().Add(setupTimeout)); err != nil {
		stream.Close()
		return nil, fmt.Errorf("failed to set tunnel setup deadline: %w", err)
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
	if err := stream.SetDeadline(time.Time{}); err != nil {
		stream.Close()
		return nil, fmt.Errorf("failed to clear tunnel setup deadline: %w", err)
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
