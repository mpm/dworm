package protocol

import (
	"bytes"
	"log"
	"strings"
	"testing"
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

	config.Logger.Printf("connection failed: %s", "test")
	if got := output.String(); !strings.Contains(got, "[transport] connection failed: test") {
		t.Fatalf("diagnostic was not routed through logger: %q", got)
	}
}
