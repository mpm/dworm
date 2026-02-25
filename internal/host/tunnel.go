package host

import (
	"fmt"
	"log"
	"net"
	"sync"

	"github.com/mpm/dworm/internal/protocol"
)

// PortMapping represents a port forwarding from container to host
type PortMapping struct {
	ContainerPort int
	LocalPort     int
}

// TunnelManager manages port forwarding from container to host
type TunnelManager struct {
	endpoint     *EndpointManager
	listeners    map[int]net.Listener
	mu           sync.Mutex
	logger       *log.Logger
	portUpdateCh chan<- []PortMapping
	bindAddr     string
}

// NewTunnelManager creates a new tunnel manager.
// bindAddr specifies the address to bind forwarded ports to (e.g., "127.0.0.1" or "0.0.0.0").
// logger is used for tunnel log messages; it must not be nil.
func NewTunnelManager(endpoint *EndpointManager, bindAddr string, logger *log.Logger) *TunnelManager {
	if bindAddr == "" {
		bindAddr = "127.0.0.1"
	}
	return &TunnelManager{
		endpoint:  endpoint,
		listeners: make(map[int]net.Listener),
		logger:    logger,
		bindAddr:  bindAddr,
	}
}

// SetPortUpdateChannel sets the channel for port update notifications
func (t *TunnelManager) SetPortUpdateChannel(ch chan<- []PortMapping) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.portUpdateCh = ch
}

// notifyPortChange sends current port mappings to the update channel
func (t *TunnelManager) notifyPortChange() {
	if t.portUpdateCh == nil {
		return
	}

	mappings := make([]PortMapping, 0, len(t.listeners))
	for containerPort, listener := range t.listeners {
		localPort := listener.Addr().(*net.TCPAddr).Port
		mappings = append(mappings, PortMapping{
			ContainerPort: containerPort,
			LocalPort:     localPort,
		})
	}

	// Non-blocking send
	select {
	case t.portUpdateCh <- mappings:
	default:
	}
}

// ForwardPort starts forwarding a port from the container to the host
func (t *TunnelManager) ForwardPort(port int) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	err := t.forwardPortLocked(port)
	if err == nil {
		t.notifyPortChange()
	}
	return err
}

// forwardPortLocked is the internal version that assumes the lock is held
func (t *TunnelManager) forwardPortLocked(port int) error {
	// Check if already forwarding
	if _, exists := t.listeners[port]; exists {
		return nil
	}

	// Try to bind to the same port on the configured address
	listener, err := net.Listen("tcp", fmt.Sprintf("%s:%d", t.bindAddr, port))
	if err != nil {
		// Try a different port if the original is in use
		listener, err = net.Listen("tcp", fmt.Sprintf("%s:0", t.bindAddr))
		if err != nil {
			return fmt.Errorf("failed to create listener: %w", err)
		}
		actualPort := listener.Addr().(*net.TCPAddr).Port
		t.logger.Printf("Port %d is in use, using %d instead", port, actualPort)
	}

	t.listeners[port] = listener
	t.logger.Printf("Forwarding port %d -> container:%d", listener.Addr().(*net.TCPAddr).Port, port)

	go t.acceptConnections(listener, port)

	return nil
}

// StopForwarding stops forwarding a port
func (t *TunnelManager) StopForwarding(port int) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if listener, exists := t.listeners[port]; exists {
		listener.Close()
		delete(t.listeners, port)
		t.logger.Printf("Stopped forwarding port %d", port)
	}

	return nil
}

// UpdatePorts updates the forwarded ports based on what's listening in the container
func (t *TunnelManager) UpdatePorts(ports []protocol.PortInfo) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Build set of new ports for quick lookup (by port number only)
	newPortSet := make(map[int]struct{})
	for _, p := range ports {
		newPortSet[p.Port] = struct{}{}
	}

	// Remove ports that are no longer listening
	for port, listener := range t.listeners {
		if _, exists := newPortSet[port]; !exists {
			listener.Close()
			delete(t.listeners, port)
			t.logger.Printf("Stopped forwarding port %d", port)
		}
	}

	// Add new ports (using internal method that doesn't acquire lock)
	for _, p := range ports {
		if _, exists := t.listeners[p.Port]; !exists {
			if err := t.forwardPortLocked(p.Port); err != nil {
				t.logger.Printf("Failed to forward port %d: %v", p.Port, err)
			}
		}
	}

	// Notify about the port change
	t.notifyPortChange()
}

// GetForwardedPorts returns a map of container port -> local port
func (t *TunnelManager) GetForwardedPorts() map[int]int {
	t.mu.Lock()
	defer t.mu.Unlock()

	result := make(map[int]int)
	for containerPort, listener := range t.listeners {
		localPort := listener.Addr().(*net.TCPAddr).Port
		result[containerPort] = localPort
	}
	return result
}

// Close stops all port forwarding
func (t *TunnelManager) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()

	for port, listener := range t.listeners {
		listener.Close()
		delete(t.listeners, port)
		t.logger.Printf("Stopped forwarding port %d", port)
	}
}

func (t *TunnelManager) acceptConnections(listener net.Listener, containerPort int) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			// Listener was closed
			return
		}

		go t.handleConnection(conn, containerPort)
	}
}

func (t *TunnelManager) handleConnection(localConn net.Conn, containerPort int) {
	defer localConn.Close()

	// Open tunnel stream to endpoint
	remoteConn, err := t.endpoint.OpenTunnelStream(containerPort)
	if err != nil {
		t.logger.Printf("Failed to open tunnel to port %d: %v", containerPort, err)
		return
	}
	defer remoteConn.Close()

	// Proxy data bidirectionally
	protocol.BiProxy(localConn, remoteConn)
}
