package endpoint

import (
	"io"
	"log"
	"net"
	"sync"
	"testing"

	"github.com/mpm/dworm/internal/protocol"
)

func TestConcurrentPortUpdatesAndTunnelConnections(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	port := listener.Addr().(*net.TCPAddr).Port
	server := NewServer()
	server.logger = log.New(io.Discard, "", 0)
	server.updatePortState([]protocol.PortInfo{{Port: port, Address: "127.0.0.1"}})

	const attempts = 100
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for range attempts {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := range attempts {
			address := "127.0.0.1"
			if i%2 == 0 {
				address = "0.0.0.0"
			}
			server.updatePortState([]protocol.PortInfo{{Port: port, Address: address}})
		}
	}()
	go func() {
		defer wg.Done()
		for range attempts {
			endpointConn, hostConn := net.Pipe()
			done := make(chan struct{})
			go func() {
				server.handleTunnelStream(endpointConn)
				close(done)
			}()

			portHeader := []byte{byte(port >> 24), byte(port >> 16), byte(port >> 8), byte(port)}
			if _, err := hostConn.Write(portHeader); err != nil {
				t.Errorf("write port header: %v", err)
				hostConn.Close()
				<-done
				continue
			}
			response := make([]byte, 1)
			if _, err := io.ReadFull(hostConn, response); err != nil {
				t.Errorf("read tunnel response: %v", err)
			} else if response[0] != 1 {
				t.Errorf("tunnel response = %d, want 1", response[0])
			}
			hostConn.Close()
			<-done
		}
	}()
	wg.Wait()
	<-acceptDone
}

func TestPortStatePublishesCompleteSnapshot(t *testing.T) {
	server := NewServer()
	ports := []protocol.PortInfo{
		{Port: 3000, Address: "127.0.0.1"},
		{Port: 3000, Address: "0.0.0.0"},
		{Port: 8080, Address: "::1"},
	}

	if _, changed := server.updatePortState(ports); !changed {
		t.Fatal("initial state was not reported as changed")
	}
	if address, ok := server.portAddress(3000); !ok || address != "0.0.0.0" {
		t.Fatalf("port 3000 address = %q, %v; want 0.0.0.0, true", address, ok)
	}
	if address, ok := server.portAddress(8080); !ok || address != "::1" {
		t.Fatalf("port 8080 address = %q, %v; want ::1, true", address, ok)
	}
}
