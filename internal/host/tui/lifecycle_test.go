package tui

import (
	"errors"
	"os/exec"
	"testing"
	"time"
)

func TestWaitForCommandReturnsTransportFailure(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 10")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start command: %v", err)
	}

	wantErr := errors.New("endpoint transport failed")
	failureCh := make(chan error, 1)
	failureCh <- wantErr

	start := time.Now()
	cmdErr, failureErr := waitForCommand(cmd, failureCh)
	if !errors.Is(failureErr, wantErr) {
		t.Fatalf("failure error = %v, want %v", failureErr, wantErr)
	}
	if cmdErr == nil {
		t.Fatal("command was not terminated")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("transport failure took %v to terminate command", elapsed)
	}
}

func TestWaitForCommandAllowsNormalExit(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start command: %v", err)
	}

	cmdErr, failureErr := waitForCommand(cmd, make(chan error))
	if cmdErr != nil {
		t.Fatalf("command error: %v", cmdErr)
	}
	if failureErr != nil {
		t.Fatalf("failure error: %v", failureErr)
	}
}
