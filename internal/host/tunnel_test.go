package host

import (
	"errors"
	"io"
	"log"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/mpm/dworm/internal/protocol"
)

func TestTunnelManagerRetriesListenerCreation(t *testing.T) {
	manager := NewTunnelManager(nil, "127.0.0.1", log.New(io.Discard, "", 0))
	manager.retryDelay = 10 * time.Millisecond
	defer manager.Close()

	var mu sync.Mutex
	attempts := 0
	manager.listen = func(network, address string) (net.Listener, error) {
		mu.Lock()
		defer mu.Unlock()
		attempts++
		if attempts <= 2 {
			return nil, errors.New("temporary bind failure")
		}
		return net.Listen(network, address)
	}

	manager.UpdatePorts([]protocol.PortInfo{{Port: 18080, Address: "127.0.0.1"}})
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, ok := manager.GetForwardedPorts()[18080]; ok {
			mu.Lock()
			gotAttempts := attempts
			mu.Unlock()
			if gotAttempts < 3 {
				t.Fatalf("listen attempts = %d, want at least 3", gotAttempts)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("listener was not created after transient failure")
}

func TestTunnelManagerClosesActiveConnectionsWhenPortDisappears(t *testing.T) {
	manager := NewTunnelManager(nil, "127.0.0.1", log.New(io.Discard, "", 0))
	defer manager.Close()
	const port = 18080
	manager.mu.Lock()
	manager.desiredPorts[port] = struct{}{}
	manager.mu.Unlock()

	proxyConn, peerConn := net.Pipe()
	defer peerConn.Close()
	if !manager.registerConnection(port, proxyConn) {
		t.Fatal("failed to register active connection")
	}

	manager.UpdatePorts(nil)
	if _, err := peerConn.Read(make([]byte, 1)); err == nil {
		t.Fatal("active connection did not report EOF after port removal")
	}
}

func TestTunnelManagerConnectionCompletionUnregistersSafely(t *testing.T) {
	manager := NewTunnelManager(nil, "127.0.0.1", log.New(io.Discard, "", 0))
	defer manager.Close()
	const port = 18080
	manager.mu.Lock()
	manager.desiredPorts[port] = struct{}{}
	manager.mu.Unlock()

	var wg sync.WaitGroup
	for range 100 {
		proxyConn, peerConn := net.Pipe()
		if !manager.registerConnection(port, proxyConn) {
			t.Fatal("failed to register active connection")
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			manager.unregisterConnection(port, proxyConn)
			proxyConn.Close()
			peerConn.Close()
		}()
	}
	manager.UpdatePorts(nil)
	wg.Wait()

	manager.mu.Lock()
	defer manager.mu.Unlock()
	if len(manager.activeConns) != 0 {
		t.Fatalf("active connections not cleared: %v", manager.activeConns)
	}
}
