package host

import (
	"io"
	"testing"
	"time"

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
			portHeader := make([]byte, 4)
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
		portHeader := make([]byte, 4)
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
