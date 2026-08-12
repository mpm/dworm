package host

import (
	"fmt"
	"log"
	"net"
	"sync"
	"time"

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
	desiredPorts map[int]struct{}
	retryTimers  map[int]*time.Timer
	retryDelay   time.Duration
	listen       func(string, string) (net.Listener, error)
	closed       bool
	activeConns  map[int]map[net.Conn]struct{}
}

// NewTunnelManager creates a new tunnel manager.
// bindAddr specifies the address to bind forwarded ports to (e.g., "127.0.0.1" or "0.0.0.0").
// logger is used for tunnel log messages; it must not be nil.
func NewTunnelManager(endpoint *EndpointManager, bindAddr string, logger *log.Logger) *TunnelManager {
	if bindAddr == "" {
		bindAddr = "127.0.0.1"
	}
	return &TunnelManager{
		endpoint:     endpoint,
		listeners:    make(map[int]net.Listener),
		logger:       logger,
		bindAddr:     bindAddr,
		desiredPorts: make(map[int]struct{}),
		retryTimers:  make(map[int]*time.Timer),
		retryDelay:   time.Second,
		listen:       net.Listen,
		activeConns:  make(map[int]map[net.Conn]struct{}),
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
	t.desiredPorts[port] = struct{}{}
	err := t.forwardPortLocked(port)
	if err == nil {
		t.notifyPortChange()
	} else {
		t.scheduleRetryLocked(port)
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
	listener, err := t.listen("tcp", fmt.Sprintf("%s:%d", t.bindAddr, port))
	if err != nil {
		// Try a different port if the original is in use
		listener, err = t.listen("tcp", fmt.Sprintf("%s:0", t.bindAddr))
		if err != nil {
			return fmt.Errorf("failed to create listener: %w", err)
		}
		actualPort := listener.Addr().(*net.TCPAddr).Port
		t.logger.Printf("Port %d is in use, using %d instead", port, actualPort)
	}

	t.listeners[port] = listener
	if timer := t.retryTimers[port]; timer != nil {
		timer.Stop()
		delete(t.retryTimers, port)
	}
	t.logger.Printf("Forwarding port %d -> container:%d", listener.Addr().(*net.TCPAddr).Port, port)

	go t.acceptConnections(listener, port)

	return nil
}

// StopForwarding stops forwarding a port
func (t *TunnelManager) StopForwarding(port int) error {
	t.mu.Lock()

	if listener, exists := t.listeners[port]; exists {
		listener.Close()
		delete(t.listeners, port)
		t.logger.Printf("Stopped forwarding port %d", port)
	}
	delete(t.desiredPorts, port)
	t.cancelRetryLocked(port)
	connections := t.takeActiveLocked(port)
	t.mu.Unlock()
	closeConnections(connections)

	return nil
}

// UpdatePorts updates the forwarded ports based on what's listening in the container
func (t *TunnelManager) UpdatePorts(ports []protocol.PortInfo) {
	t.mu.Lock()
	var connectionsToClose []net.Conn

	// Build set of new ports for quick lookup (by port number only)
	newPortSet := make(map[int]struct{})
	for _, p := range ports {
		newPortSet[p.Port] = struct{}{}
	}
	t.desiredPorts = newPortSet

	// Remove ports that are no longer listening
	for port, listener := range t.listeners {
		if _, exists := newPortSet[port]; !exists {
			listener.Close()
			delete(t.listeners, port)
			connectionsToClose = append(connectionsToClose, t.takeActiveLocked(port)...)
			t.logger.Printf("Stopped forwarding port %d", port)
		}
	}
	for port := range t.activeConns {
		if _, exists := newPortSet[port]; !exists {
			connectionsToClose = append(connectionsToClose, t.takeActiveLocked(port)...)
		}
	}
	for port := range t.retryTimers {
		if _, exists := newPortSet[port]; !exists {
			t.cancelRetryLocked(port)
		}
	}

	// Add new ports (using internal method that doesn't acquire lock)
	for _, p := range ports {
		if _, exists := t.listeners[p.Port]; !exists {
			if err := t.forwardPortLocked(p.Port); err != nil {
				t.logger.Printf("Failed to forward port %d: %v", p.Port, err)
				t.scheduleRetryLocked(p.Port)
			}
		}
	}

	// Notify about the port change
	t.notifyPortChange()
	t.mu.Unlock()
	closeConnections(connectionsToClose)
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

	t.closed = true
	for port := range t.retryTimers {
		t.cancelRetryLocked(port)
	}
	var connectionsToClose []net.Conn
	for port, listener := range t.listeners {
		listener.Close()
		delete(t.listeners, port)
		t.logger.Printf("Stopped forwarding port %d", port)
	}
	for port := range t.activeConns {
		connectionsToClose = append(connectionsToClose, t.takeActiveLocked(port)...)
	}
	t.mu.Unlock()
	closeConnections(connectionsToClose)
}

func (t *TunnelManager) scheduleRetryLocked(port int) {
	if t.closed || t.retryTimers[port] != nil {
		return
	}
	if _, desired := t.desiredPorts[port]; !desired {
		return
	}

	t.retryTimers[port] = time.AfterFunc(t.retryDelay, func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		delete(t.retryTimers, port)
		if t.closed {
			return
		}
		if _, desired := t.desiredPorts[port]; !desired {
			return
		}
		if err := t.forwardPortLocked(port); err != nil {
			t.logger.Printf("Failed to retry port %d: %v", port, err)
			t.scheduleRetryLocked(port)
			return
		}
		t.notifyPortChange()
	})
}

func (t *TunnelManager) cancelRetryLocked(port int) {
	if timer := t.retryTimers[port]; timer != nil {
		timer.Stop()
		delete(t.retryTimers, port)
	}
}

func (t *TunnelManager) registerConnection(port int, conn net.Conn) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return false
	}
	if _, desired := t.desiredPorts[port]; !desired {
		return false
	}
	connections := t.activeConns[port]
	if connections == nil {
		connections = make(map[net.Conn]struct{})
		t.activeConns[port] = connections
	}
	connections[conn] = struct{}{}
	return true
}

func (t *TunnelManager) unregisterConnection(port int, conn net.Conn) {
	t.mu.Lock()
	defer t.mu.Unlock()
	connections := t.activeConns[port]
	delete(connections, conn)
	if len(connections) == 0 {
		delete(t.activeConns, port)
	}
}

func (t *TunnelManager) takeActiveLocked(port int) []net.Conn {
	connections := t.activeConns[port]
	delete(t.activeConns, port)
	result := make([]net.Conn, 0, len(connections))
	for conn := range connections {
		result = append(result, conn)
	}
	return result
}

func closeConnections(connections []net.Conn) {
	for _, conn := range connections {
		conn.Close()
	}
}

func (t *TunnelManager) acceptConnections(listener net.Listener, containerPort int) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			// Listener was closed
			return
		}

		if !t.registerConnection(containerPort, conn) {
			conn.Close()
			continue
		}
		go t.handleConnection(conn, containerPort)
	}
}

func (t *TunnelManager) handleConnection(localConn net.Conn, containerPort int) {
	defer localConn.Close()
	defer t.unregisterConnection(containerPort, localConn)

	// Open tunnel stream to endpoint
	remoteConn, err := t.endpoint.OpenTunnelStream(containerPort)
	if err != nil {
		t.logger.Printf("Failed to open tunnel to port %d: %v", containerPort, err)
		return
	}
	defer remoteConn.Close()
	if !t.registerConnection(containerPort, remoteConn) {
		return
	}
	defer t.unregisterConnection(containerPort, remoteConn)

	// Proxy data bidirectionally
	if err := protocol.BiProxy(localConn, remoteConn); err != nil {
		t.logger.Printf("Tunnel to port %d closed with error: %v", containerPort, err)
	}
}
