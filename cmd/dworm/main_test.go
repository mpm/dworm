package main

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mpm/dworm/internal/host"
	"github.com/spf13/cobra"
)

func TestOperationalErrorDoesNotPrintUsage(t *testing.T) {
	root := newTestCommand(func(cmd *cobra.Command, args []string) error {
		return errors.New("container creation failed")
	})
	root.SetArgs([]string{"up"})

	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)

	if err := root.Execute(); err == nil {
		t.Fatal("Execute() returned nil, want an operational error")
	}
	if strings.Contains(output.String(), "Usage:") {
		t.Fatalf("operational error printed usage:\n%s", output.String())
	}
}

func TestInvalidArgumentsStillPrintUsage(t *testing.T) {
	root := newTestCommand(func(cmd *cobra.Command, args []string) error {
		t.Fatal("RunE called despite invalid arguments")
		return nil
	})
	root.SetArgs([]string{"up", "unexpected"})

	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)

	if err := root.Execute(); err == nil {
		t.Fatal("Execute() returned nil, want an argument error")
	}
	if !strings.Contains(output.String(), "Usage:") {
		t.Fatalf("argument error did not print usage:\n%s", output.String())
	}
}

func newTestCommand(runE func(*cobra.Command, []string) error) *cobra.Command {
	root := &cobra.Command{Use: "dworm"}
	root.AddCommand(&cobra.Command{
		Use:  "up",
		Args: cobra.NoArgs,
		RunE: runOperationalCommand(runE),
	})
	return root
}

func TestFormatEvent(t *testing.T) {
	ts := time.Date(2026, 10, 7, 10, 0, 0, 0, time.Local)
	for _, tt := range []struct {
		event host.Event
		want  string
	}{
		{host.Event{Type: host.EventLog, Source: "endpoint", Message: "hello"}, "[endpoint] hello"},
		{host.Event{Type: host.EventState, State: "reconnecting", Reason: "bridge_lost"}, "state: reconnecting (bridge_lost)"},
		{host.Event{Type: host.EventPorts, Ports: []host.StatePort{{Port: 3000, Address: "127.0.0.1", LocalPort: 3001}}}, "ports: 127.0.0.1:3001->3000"},
		{host.Event{Type: host.EventExecStarted, ID: "ab", Argv: []string{"opencode", "acp"}}, "exec ab started: opencode acp"},
		{host.Event{Type: host.EventExecExited, ID: "ab", Code: 255, Error: "bridge lost"}, "exec ab ended: bridge lost"},
		{host.Event{Type: host.EventExecExited, ID: "ab", Code: 143, Signal: "TERM"}, "exec ab exited: signal TERM"},
	} {
		tt.event.T = ts
		if got := formatEvent(tt.event); got != "2026-10-07 10:00:00 "+tt.want {
			t.Errorf("formatEvent = %q, want %q", got, tt.want)
		}
	}
}

func TestExitCodeFor(t *testing.T) {
	notReady := &host.ExecRejectedError{Message: "not ready", Code: host.ReplyCodeNotReady}
	for _, tt := range []struct {
		err  error
		exec bool
		want int
	}{
		{&host.ExitError{Code: 7}, true, 7},               // the command's own status
		{&host.ExitError{Code: host.ExitLost}, true, 255}, // started, then lost
		{fmt.Errorf("wrapped: %w", &host.ExitError{Code: 2}), true, 2},
		{&exitCodeError{code: exitAlreadyRunning, err: errors.New("running")}, false, 3},
		{errors.New("timed out after 15m0s waiting for the dworm instance"), true, exitExecFailed},
		{notReady, true, exitExecFailed},
		{host.ErrContainerNotRunning, true, exitExecFailed},
		{errors.New("failed"), false, 1},
	} {
		if got := exitCodeFor(tt.err, tt.exec); got != tt.want {
			t.Errorf("exitCodeFor(%v, exec=%v) = %d, want %d", tt.err, tt.exec, got, tt.want)
		}
	}
	if exitExecFailed != 125 {
		t.Fatalf("exitExecFailed = %d; it is documented as 125", exitExecFailed)
	}
}

func TestExecTTYFlags(t *testing.T) {
	for _, tt := range []struct {
		args                     []string
		terminals, want, invalid bool
	}{
		{nil, true, true, false},
		{nil, false, false, false},
		{[]string{"-t"}, false, true, false},
		{[]string{"--tty"}, true, true, false},
		{[]string{"-T"}, true, false, false},
		{[]string{"--no-tty"}, false, false, false},
		{[]string{"--tty=false"}, true, false, false},
		{[]string{"-t", "-T"}, true, false, true},
	} {
		t.Run(fmt.Sprint(tt.args, tt.terminals), func(t *testing.T) {
			cmd := &cobra.Command{Use: "exec", RunE: func(cmd *cobra.Command, _ []string) error {
				if got := execWantsTTY(cmd, tt.terminals); got != tt.want {
					t.Fatalf("TTY = %v, want %v", got, tt.want)
				}
				return nil
			}}
			addExecTTYFlags(cmd)
			cmd.SetArgs(tt.args)
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			err := cmd.Execute()
			if (err != nil) != tt.invalid {
				t.Fatalf("Execute() = %v, invalid = %v", err, tt.invalid)
			}
		})
	}
}
