//go:build linux

package endpoint

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/mpm/dworm/internal/host"
	"github.com/mpm/dworm/internal/host/tui"
	"github.com/mpm/dworm/internal/protocol"
	"golang.org/x/term"
)

// Re-execute a real terminal client in its own process/session so local raw
// mode and signal handlers are tested without changing the test runner's stdio.
func TestTTYClientHelper(t *testing.T) {
	mode := os.Getenv("DWORM_TTY_HELPER")
	if mode == "" {
		return
	}
	argv := []string{"sh", "-c", os.Getenv("DWORM_TTY_SCRIPT")}
	if os.Getenv("DWORM_TTY_SCRIPT") == "reject" {
		argv = []string{"/dworm-test-command-does-not-exist"}
	}
	socket := os.Getenv("DWORM_TTY_SOCKET")
	var err error
	if mode == "shell" {
		err = tui.Run(tui.Config{SocketPath: socket, Argv: argv, ReconnectTimeout: 500 * time.Millisecond})
	} else {
		err = host.ExecTTY(socket, protocol.ExecRequest{Argv: argv})
	}
	var exit *host.ExitError
	if errors.As(err, &exit) {
		os.Exit(exit.Code)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(120)
	}
	os.Exit(0)
}

type ttyClient struct {
	cmd           *exec.Cmd
	master, slave *os.File
	initial       *term.State
	mu            sync.Mutex
	output        bytes.Buffer
	stderr        bytes.Buffer
	wait          chan error
}

func startTTYClient(t *testing.T, mode, socket, script string) *ttyClient {
	t.Helper()
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	c := &ttyClient{master: master, slave: slave, wait: make(chan error, 1)}
	t.Cleanup(func() { master.Close(); slave.Close() })
	if err := pty.Setsize(master, &pty.Winsize{Rows: 24, Cols: 80}); err != nil {
		t.Fatal(err)
	}
	c.initial, err = term.GetState(int(slave.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	c.cmd = exec.Command(os.Args[0], "-test.run=^TestTTYClientHelper$")
	c.cmd.Env = append(os.Environ(), "DWORM_TTY_HELPER="+mode, "DWORM_TTY_SOCKET="+socket, "DWORM_TTY_SCRIPT="+script, "TERM=xterm-256color")
	c.cmd.Stdin, c.cmd.Stdout, c.cmd.Stderr = slave, slave, &c.stderr
	c.cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := c.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.cmd.Process.Kill() })
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
			c.mu.Lock()
			c.output.Write(buf[:n])
			c.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	go func() { c.wait <- c.cmd.Wait() }()
	return c
}

func (c *ttyClient) readUntil(t *testing.T, text string) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		found := strings.Contains(c.output.String(), text)
		c.mu.Unlock()
		if found {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	t.Fatalf("waiting for %q; output: %q", text, c.output.String())
}

func (c *ttyClient) finish(t *testing.T, code int) {
	t.Helper()
	select {
	case err := <-c.wait:
		got := 0
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			got = exit.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		if got != code {
			t.Fatalf("exit = %d, want %d; stderr: %s", got, code, c.stderr.String())
		}
	case <-time.After(8 * time.Second):
		t.Fatal("terminal client hung")
	}
	state, err := term.GetState(int(c.slave.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(state, c.initial) {
		t.Fatal("host terminal was not restored to its original state")
	}
}

func TestTTYClientsOverInstanceSocket(t *testing.T) {
	for _, mode := range []string{"exec", "shell"} {
		scenarios := []string{"resize and exit", "control bytes", "bridge lost", "instance stopped", "start rejected", "SIGTERM", "command exits 255"}
		if mode == "shell" {
			scenarios = append(scenarios, "reconnect new shell", "events lost")
		}
		for _, scenario := range scenarios {
			t.Run(mode+"/"+scenario, func(t *testing.T) {
				f := newExecFixture(t)
				bus := host.NewEventBus()
				bus.Publish(host.Event{Type: host.EventState, State: host.StateReady})
				var ready atomic.Bool
				ready.Store(true)
				var active atomic.Pointer[protocol.Mux]
				active.Store(f.h.HostMux)
				socket, err := host.NewExecServer(filepath.Join(t.TempDir(), "exec.sock"), host.ExecServerConfig{
					Open: func() (net.Conn, error) { return active.Load().OpenStream() }, Events: bus, Ready: ready.Load,
				})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { socket.Close(); bus.Close() })
				script := `stty -echo; echo READY; while :; do sleep 0.05; done`
				switch scenario {
				case "resize and exit":
					script = `stty -echo; trap 'printf "RESIZED="; stty size' WINCH; printf "INITIAL="; stty size; while :; do read x; [ "$x" = exit ] && exit 37; done`
				case "control bytes":
					script = `stty raw -echo; echo READY; dd bs=1 count=3 2>/dev/null | od -An -tx1; exit 19`
				case "start rejected":
					script = "reject"
				case "SIGTERM":
					script = `trap 'exit 43' TERM; echo READY; while :; do sleep 0.05; done`
				case "command exits 255":
					script = `stty -echo; echo READY; read x; exit 255`
				case "reconnect new shell":
					script = `stty -echo; printf "INITIAL="; stty size; echo READY; read x; exit 42`
				}
				c := startTTYClient(t, mode, socket.Path(), script)
				if scenario == "start rejected" {
					// The TUI starts before raw mode; ExecTTY enters raw mode first.
					c.finish(t, 127)
					return
				}
				if scenario == "resize and exit" {
					rows := 24
					if mode == "shell" {
						rows--
					}
					c.readUntil(t, fmt.Sprintf("INITIAL=%d 80", rows))
				} else {
					c.readUntil(t, "READY")
				}
				state, err := term.GetState(int(c.slave.Fd()))
				if err != nil || reflect.DeepEqual(state, c.initial) {
					t.Fatal("client did not put host terminal in raw mode")
				}
				switch scenario {
				case "resize and exit":
					if err := pty.Setsize(c.master, &pty.Winsize{Rows: 40, Cols: 100}); err != nil {
						t.Fatal(err)
					}
					c.cmd.Process.Signal(syscall.SIGWINCH)
					rows := 40
					if mode == "shell" {
						rows--
					}
					c.readUntil(t, fmt.Sprintf("RESIZED=%d 100", rows))
					c.master.Write([]byte("exit\r"))
					c.finish(t, 37)
				case "control bytes":
					// These must be bytes, not host signals (including SIGTSTP).
					c.master.Write([]byte{0x03, 0x1a, 0x1c})
					c.readUntil(t, "03 1a 1c")
					c.finish(t, 19)
				case "bridge lost":
					ready.Store(false)
					bus.Publish(host.Event{Type: host.EventState, State: host.StateReconnecting})
					f.h.HostMux.Close()
					c.finish(t, host.ExitLost)
					if !strings.HasPrefix(c.stderr.String(), "dworm: ") || strings.Count(c.stderr.String(), "\n") != 1 {
						t.Fatalf("loss diagnostic: %q", c.stderr.String())
					}
				case "instance stopped":
					bus.Publish(host.Event{Type: host.EventState, State: host.StateStopped})
					if mode == "exec" {
						socket.Close()
					}
					c.finish(t, host.ExitLost)
				case "SIGTERM":
					c.cmd.Process.Signal(syscall.SIGTERM)
					code := 43
					if mode == "shell" {
						code = 143
					}
					c.finish(t, code)
				case "command exits 255":
					c.master.Write([]byte("exit\r"))
					c.finish(t, 255)
					if c.stderr.Len() != 0 {
						t.Fatalf("command exit 255 misreported as loss: %q", c.stderr.String())
					}
				case "events lost":
					bus.Close()
					c.finish(t, host.ExitLost)
				case "reconnect new shell":
					f2 := newExecFixture(t)
					ready.Store(false)
					bus.Publish(host.Event{Type: host.EventState, State: host.StateReconnecting})
					f.h.HostMux.Close()
					c.readUntil(t, "Connection to the container lost")
					if err := pty.Setsize(c.master, &pty.Winsize{Rows: 40, Cols: 100}); err != nil {
						t.Fatal(err)
					}
					c.cmd.Process.Signal(syscall.SIGWINCH)
					c.readUntil(t, "\x1b[1;39r")
					active.Store(f2.h.HostMux)
					ready.Store(true)
					bus.Publish(host.Event{Type: host.EventState, State: host.StateReady})
					c.readUntil(t, "Reconnected; this is a new shell")
					c.readUntil(t, "INITIAL=39 100")
					c.master.Write([]byte("exit\r"))
					c.finish(t, 42)
					if c.stderr.Len() != 0 {
						t.Fatalf("successful reconnect stderr: %q", c.stderr.String())
					}
				}
			})
		}
	}
}

func TestTTYExplicitWithoutLocalTerminal(t *testing.T) {
	f := newExecFixture(t)
	socket, err := host.NewExecServer(filepath.Join(t.TempDir(), "exec.sock"), host.ExecServerConfig{Open: f.h.HostMux.OpenStream})
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestTTYClientHelper$")
	cmd.Env = append(os.Environ(), "DWORM_TTY_HELPER=exec", "DWORM_TTY_SOCKET="+socket.Path(), "DWORM_TTY_SCRIPT=test -t 0 && test -t 1 && echo PTY; stty size; exit 23")
	cmd.Stdin = strings.NewReader("")
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 23 || !bytes.Contains(out, []byte("PTY")) || !bytes.Contains(out, []byte("24 80")) {
		t.Fatalf("explicit PTY with pipes: output %q, error %v", out, err)
	}
}
