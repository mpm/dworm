package protocol

import (
	"bytes"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
)

func TestYamuxConfigUsesProvidedLogger(t *testing.T) {
	var output bytes.Buffer
	logger := log.New(&output, "[transport] ", 0)
	config := newYamuxConfig(logger)

	if config.LogOutput != nil {
		t.Fatal("yamux LogOutput should be disabled when using a logger")
	}
	if config.Logger != logger {
		t.Fatal("yamux logger was not configured with the provided logger")
	}

	if err := yamux.VerifyConfig(config); err != nil {
		t.Fatalf("invalid yamux config: %v", err)
	}

	config.Logger.Printf("connection failed: %s", "test")
	if got := output.String(); !strings.Contains(got, "[transport] connection failed: test") {
		t.Fatalf("diagnostic was not routed through logger: %q", got)
	}
}

// A host stalled for minutes (memory pressure) must not tear down the bridge:
// the 2026-10-21 stalls lasted ~3 minutes and yamux's defaults gave up after 10s.
func TestYamuxConfigToleratesHostStalls(t *testing.T) {
	config := newYamuxConfig(log.New(io.Discard, "", 0))

	if !config.EnableKeepAlive {
		t.Fatal("keepalive should stay enabled to detect a hung peer")
	}
	if config.ConnectionWriteTimeout != BridgeStallTimeout {
		t.Fatalf("ConnectionWriteTimeout = %v, want %v", config.ConnectionWriteTimeout, BridgeStallTimeout)
	}
	if config.StreamOpenTimeout != BridgeStallTimeout {
		t.Fatalf("StreamOpenTimeout = %v, want %v", config.StreamOpenTimeout, BridgeStallTimeout)
	}
	if BridgeStallTimeout < 3*time.Minute {
		t.Fatalf("BridgeStallTimeout = %v, shorter than the observed stalls", BridgeStallTimeout)
	}
}
