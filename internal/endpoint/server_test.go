package endpoint

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mpm/dworm/internal/protocol"
	"github.com/mpm/dworm/internal/protocol/testutil"
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

func TestFailedPortUpdateIsRetried(t *testing.T) {
	server := NewServer()
	server.logger = log.New(io.Discard, "", 0)
	ports := []protocol.PortInfo{{Port: 8080, Address: "127.0.0.1"}}

	attempts := 0
	server.sendControl = func(string, interface{}) error {
		attempts++
		if attempts == 1 {
			return errors.New("temporary send failure")
		}
		return nil
	}

	server.reportPorts(ports)
	if got := server.currentPortSnapshot(); len(got) != 0 {
		t.Fatalf("failed update was committed: %v", got)
	}
	server.reportPorts(ports)
	if attempts != 2 {
		t.Fatalf("send attempts = %d, want 2", attempts)
	}
	if got := server.currentPortSnapshot(); len(got) != 1 || got[0] != ports[0] {
		t.Fatalf("successful update was not committed: %v", got)
	}
}

func TestFirstPortReportIsSentWhenEmpty(t *testing.T) {
	server := NewServer()
	server.logger = log.New(io.Discard, "", 0)
	sends := 0
	server.sendControl = func(string, interface{}) error {
		sends++
		return nil
	}
	server.reportPorts(nil)
	server.reportPorts(nil)
	if sends != 1 {
		t.Fatalf("sends = %d, want exactly the first (empty) report", sends)
	}
}

func TestLogsGoOverControlChannelAfterInit(t *testing.T) {
	old := environmentDir
	environmentDir = filepath.Join(t.TempDir(), "dworm")
	t.Cleanup(func() { environmentDir = old })
	h, err := testutil.NewTestHarness()
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	var stderr syncBuffer
	server := NewServer()
	server.logWriter.stderr = &stderr
	server.mux = h.EndpointMux
	server.logger.Printf("before init")
	go server.waitForInit()
	h.HostMux.SendControl(protocol.TypeInit, &protocol.InitMessage{ProtocolVersion: protocol.ProtocolVersion})
	if kind, _, err := h.HostMux.RecvControl(); err != nil || kind != protocol.TypeEnvironmentReady {
		t.Fatalf("first message = %s, %v; want environment_ready", kind, err)
	}
	waitLog := func(want string) protocol.LogMessage {
		t.Helper()
		for {
			kind, data, err := h.HostMux.RecvControl()
			if err != nil {
				t.Fatal(err)
			}
			var msg protocol.LogMessage
			if kind == protocol.TypeLog && json.Unmarshal(data, &msg) == nil && msg.Message == want {
				return msg
			}
		}
	}
	// waitForInit logs this last, after switching to the control channel.
	waitLog("Initialized with 0 env vars")
	server.logger.Printf("Warning: after init")
	if msg := waitLog("Warning: after init"); msg.Level != protocol.LogLevelWarn {
		t.Fatalf("level = %q", msg.Level)
	}
	if got := stderr.String(); got != "before init\n" {
		t.Fatalf("stderr = %q, want only the line before init", got)
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestPortScanFailurePreservesCurrentSnapshot(t *testing.T) {
	server := NewServer()
	server.logger = log.New(io.Discard, "", 0)
	wantPorts := []protocol.PortInfo{{Port: 8080, Address: "127.0.0.1"}}
	server.updatePortState(wantPorts)
	server.scanPorts = func() ([]protocol.PortInfo, error) {
		return nil, errors.New("temporary proc failure")
	}
	sendCalled := false
	server.sendControl = func(string, interface{}) error {
		sendCalled = true
		return nil
	}

	server.scanAndReport()
	if sendCalled {
		t.Fatal("scan failure published a port update")
	}
	if got := server.currentPortSnapshot(); len(got) != 1 || got[0] != wantPorts[0] {
		t.Fatalf("snapshot changed after scan failure: %v", got)
	}
}

func TestInitRejectsProtocolVersionMismatch(t *testing.T) {
	h, err := testutil.NewTestHarness()
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	server := NewServer()
	server.logger = log.New(io.Discard, "", 0)
	server.mux = h.EndpointMux
	done := make(chan error, 1)
	go func() { done <- server.waitForInit() }()

	if err := h.HostMux.SendControl(protocol.TypeInit, &protocol.InitMessage{ProtocolVersion: protocol.ProtocolVersion - 1}); err != nil {
		t.Fatal(err)
	}
	kind, data, err := h.HostMux.RecvControl()
	if err != nil || kind != protocol.TypeInitError || !strings.Contains(string(data), "protocol version mismatch") {
		t.Fatalf("reply = %s %s, %v; want init_error", kind, data, err)
	}
	if err := <-done; err == nil {
		t.Fatal("waitForInit accepted a mismatched protocol version")
	}
}
