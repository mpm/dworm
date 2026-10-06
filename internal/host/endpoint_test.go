package host

import (
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/mpm/dworm/internal/protocol"
	"github.com/mpm/dworm/internal/protocol/testutil"
)

func TestOpenTunnelStreamTimesOutWithoutResponse(t *testing.T) {
	h, err := testutil.NewTestHarness()
	if err != nil {
		t.Fatalf("create harness: %v", err)
	}
	defer h.Close()

	endpoint := &EndpointManager{mux: h.HostMux, setupTimeout: 20 * time.Millisecond}
	accepted := make(chan struct{})
	go func() {
		stream, err := h.EndpointMux.AcceptStream()
		if err == nil {
			defer stream.Close()
			portHeader := make([]byte, 5) // marker + port
			_, _ = io.ReadFull(stream, portHeader)
			close(accepted)
			_, _ = io.Copy(io.Discard, stream)
		}
	}()

	start := time.Now()
	if _, err := endpoint.OpenTunnelStream(8080); err == nil {
		t.Fatal("expected tunnel setup timeout")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("tunnel setup took %v, want prompt timeout", elapsed)
	}
	<-accepted
}

func TestOpenTunnelStreamClearsSetupDeadline(t *testing.T) {
	h, err := testutil.NewTestHarness()
	if err != nil {
		t.Fatalf("create harness: %v", err)
	}
	defer h.Close()

	setupTimeout := 20 * time.Millisecond
	endpoint := &EndpointManager{mux: h.HostMux, setupTimeout: setupTimeout}
	go func() {
		stream, err := h.EndpointMux.AcceptStream()
		if err != nil {
			return
		}
		defer stream.Close()
		portHeader := make([]byte, 5) // marker + port
		if _, err := io.ReadFull(stream, portHeader); err != nil {
			return
		}
		if _, err := stream.Write([]byte{1}); err != nil {
			return
		}
		time.Sleep(2 * setupTimeout)
		_, _ = stream.Write([]byte("response"))
	}()

	stream, err := endpoint.OpenTunnelStream(8080)
	if err != nil {
		t.Fatalf("open tunnel: %v", err)
	}
	defer stream.Close()
	response := make([]byte, len("response"))
	if _, err := io.ReadFull(stream, response); err != nil {
		t.Fatalf("read after setup deadline: %v", err)
	}
}

func TestOpenTunnelStreamSendsTypeMarker(t *testing.T) {
	h, err := testutil.NewTestHarness()
	if err != nil {
		t.Fatalf("create harness: %v", err)
	}
	defer h.Close()

	endpoint := &EndpointManager{mux: h.HostMux}
	header := make(chan []byte, 1)
	go func() {
		stream, err := h.EndpointMux.AcceptStream()
		if err != nil {
			return
		}
		defer stream.Close()
		buf := make([]byte, 5)
		io.ReadFull(stream, buf)
		header <- buf
		stream.Write([]byte{1})
	}()
	stream, err := endpoint.OpenTunnelStream(0x01020304)
	if err != nil {
		t.Fatalf("open tunnel: %v", err)
	}
	stream.Close()
	if got := <-header; string(got) != string([]byte{protocol.StreamTypeTunnel, 1, 2, 3, 4}) {
		t.Fatalf("header = %v", got)
	}
}

func TestCheckEnvironmentReady(t *testing.T) {
	ok := fmt.Sprintf(`{"protocol_version":%d}`, protocol.ProtocolVersion)
	if err := checkEnvironmentReady(protocol.TypeEnvironmentReady, []byte(ok)); err != nil {
		t.Fatalf("matching version: %v", err)
	}
	if err := checkEnvironmentReady(protocol.TypeEnvironmentReady, nil); err == nil {
		t.Fatal("acknowledgement without version (older endpoint) accepted")
	}
	if err := checkEnvironmentReady(protocol.TypeInitError, []byte(`{"error":"protocol version mismatch"}`)); err == nil || !strings.Contains(err.Error(), "protocol version mismatch") {
		t.Fatalf("init error = %v", err)
	}
}
