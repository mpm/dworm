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
