package host

import (
	"fmt"
	"log"
	"net"
	"os"
	"sync"

	"github.com/mpm/dworm/internal/protocol"
)

// TunnelManager manages port forwarding from container to host
type TunnelManager struct {
	endpoint  *EndpointManager
	listeners map[int]net.Listener
	mu        sync.Mutex
	logger    *log.Logger
}

// NewTunnelManager creates a new tunnel manager
func NewTunnelManager(endpoint *EndpointManager) *TunnelManager {
	return &TunnelManager{
		endpoint:  endpoint,
		listeners: make(map[int]net.Listener),
		logger:    log.New(protocol.NewCRWriter(os.Stderr), "[tunnel] ", log.LstdFlags),
	}
}

// ForwardPort starts forwarding a port from the container to the host
func (t *TunnelManager) ForwardPort(port int) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.forwardPortLocked(port)
}

// forwardPortLocked is the internal version that assumes the lock is held
func (t *TunnelManager) forwardPortLocked(port int) error {
	// Check if already forwarding
	if _, exists := t.listeners[port]; exists {
		return nil
	}

	// Try to bind to the same port on localhost
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		// Try a different port if the original is in use
		listener, err = net.Listen("tcp", "127.0.0.1:0")
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
func (t *TunnelManager) UpdatePorts(ports []int) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Build set of new ports for quick lookup
	newPortSet := make(map[int]struct{})
	for _, p := range ports {
		newPortSet[p] = struct{}{}
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
	for _, port := range ports {
		if _, exists := t.listeners[port]; !exists {
			if err := t.forwardPortLocked(port); err != nil {
				t.logger.Printf("Failed to forward port %d: %v", port, err)
			}
		}
	}
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
