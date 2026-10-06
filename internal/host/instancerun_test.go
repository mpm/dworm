//go:build linux

package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/mpm/dworm/internal/protocol"
	"github.com/mpm/dworm/internal/protocol/testutil"
)

// TestMain lets Ensure tests re-execute the test binary as a detached fake
// instance (`<test binary> __instance`).
func TestMain(m *testing.M) {
	if os.Getenv("DWORM_FAKE_INSTANCE") != "" && len(os.Args) > 1 && os.Args[1] == InstanceCommand {
		os.Exit(runFakeInstance())
	}
	os.Exit(m.Run())
}

func runFakeInstance() int {
	workspace, _ := os.Getwd()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	bridge := newFakeBridge()
	deps := bridge.deps()
	deps.devcontainerUp = func(context.Context, string, string, io.Writer) (*ContainerInfo, error) {
		// Longer than the lock retry window of instances that lose the race.
		time.Sleep(500 * time.Millisecond)
		return &ContainerInfo{ContainerID: "cid", ContainerName: "fake-1", WorkspaceDir: "/workspaces/fake"}, nil
	}
	inst := newInstance(InstanceOptions{
		WorkspacePath: workspace,
		Mode:          ModeDetached,
		Version:       os.Getenv("DWORM_FAKE_VERSION"),
		Output:        os.Stderr,
	}, deps)
	err := inst.run(ctx)
	if errors.Is(err, ErrAlreadyRunning) {
		return exitAlreadyRunning
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		return 1
	}
	return 0
}

// fakeBridge stands in for `docker exec dworm_endpoint`: every connect gets a
// harness whose endpoint side answers init, reports ports, and serves execs
// like fakeExecEndpoint.
type fakeBridge struct {
	exec  *fakeExecEndpoint
	ports []protocol.PortInfo

	mu       sync.Mutex
	sessions []*testutil.TestHarness
	inits    []protocol.InitMessage
	failNext int // connects that fail before the endpoint starts
}

func newFakeBridge() *fakeBridge {
	return &fakeBridge{exec: &fakeExecEndpoint{killed: make(chan string, 100)}}
}

type harnessConn struct{ h *testutil.TestHarness }

func (c harnessConn) Mux() *protocol.Mux { return c.h.HostMux }
func (c harnessConn) Close() error       { c.h.Close(); return nil }

func (b *fakeBridge) deps() instanceDeps {
	return instanceDeps{
		devcontainerUp: func(context.Context, string, string, io.Writer) (*ContainerInfo, error) {
			return &ContainerInfo{ContainerID: "cid", ContainerName: "fake-1", WorkspaceDir: "/workspaces/fake"}, nil
		},
		startEndpoint: func(_ context.Context, _ string, _, _ io.Writer) (endpointConn, error) {
			b.mu.Lock()
			if b.failNext > 0 {
				b.failNext--
				b.mu.Unlock()
				return nil, errors.New("container is not running")
			}
			b.mu.Unlock()
			h, err := testutil.NewTestHarness()
			if err != nil {
				return nil, err
			}
			b.mu.Lock()
			b.sessions = append(b.sessions, h)
			b.mu.Unlock()
			go b.serve(h.EndpointMux)
			return harnessConn{h}, nil
		},
		containerRunning: func(string) bool { return true },
		forwarding:       func(*log.Logger) forwardingConfig { return forwardingConfig{} },
		initTimeout:      5 * time.Second,
	}
}

func (b *fakeBridge) serve(mux *protocol.Mux) {
	kind, data, err := mux.RecvControl()
	if err != nil || kind != protocol.TypeInit {
		return
	}
	var init protocol.InitMessage
	json.Unmarshal(data, &init)
	b.mu.Lock()
	b.inits = append(b.inits, init)
	b.mu.Unlock()
	mux.SendControl(protocol.TypeEnvironmentReady, &protocol.EnvironmentReadyMessage{ProtocolVersion: protocol.ProtocolVersion})
	mux.SendControl(protocol.TypePortUpdate, &protocol.PortUpdateMessage{Ports: b.ports})
	go b.exec.serve(mux)
	for {
		if _, _, err := mux.RecvControl(); err != nil {
			return
		}
	}
}

// breakBridge simulates the endpoint dying.
func (b *fakeBridge) breakBridge() {
	b.mu.Lock()
	h := b.sessions[len(b.sessions)-1]
	b.mu.Unlock()
	h.EndpointMux.Close()
}

func (b *fakeBridge) connects() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.sessions)
}

type instanceFixture struct {
	inst   *Instance
	paths  InstancePaths
	bridge *fakeBridge
	done   chan error
	cancel context.CancelFunc
}

// startInstance runs an instance with fake dependencies; adjust changes them
// before the start.
func startInstance(t *testing.T, adjust func(*instanceDeps)) *instanceFixture {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	workspace := t.TempDir()
	bridge := newFakeBridge()
	deps := bridge.deps()
	if adjust != nil {
		adjust(&deps)
	}
	inst := newInstance(InstanceOptions{WorkspacePath: workspace, Mode: ModeForeground, Version: "vtest"}, deps)
	ctx, cancel := context.WithCancel(context.Background())
	f := &instanceFixture{inst: inst, paths: InstancePathsFor(workspace), bridge: bridge, done: make(chan error, 1), cancel: cancel}
	go func() { f.done <- inst.run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-f.done:
		case <-time.After(5 * time.Second):
			t.Error("instance did not stop")
		}
	})
	waitFor(t, "instance socket", func() bool {
		_, err := QueryInstance(f.paths.Socket)
		return err == nil
	})
	return f
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitState(t *testing.T, socket, want string) *InstanceState {
	t.Helper()
	var state *InstanceState
	waitFor(t, "state "+want, func() bool {
		var err error
		state, err = QueryInstance(socket)
		return err == nil && state.State == want
	})
	return state
}

func (f *instanceFixture) wait(t *testing.T) error {
	t.Helper()
	select {
	case err := <-f.done:
		f.done <- err
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("instance did not exit")
		return nil
	}
}

func TestInstanceServesSocketBeforeReadyAndStopsOnRequest(t *testing.T) {
	release := make(chan struct{})
	f := startInstance(t, func(d *instanceDeps) {
		up := d.devcontainerUp
		d.devcontainerUp = func(ctx context.Context, ws, cfg string, progress io.Writer) (*ContainerInfo, error) {
			fmt.Fprintln(progress, "building image")
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return up(ctx, ws, cfg, progress)
		}
	})

	state := waitState(t, f.paths.Socket, StateStarting)
	if state.PID != os.Getpid() || state.Mode != ModeForeground || state.DwormVersion != "vtest" || state.ExecSocket != f.paths.Socket {
		t.Fatalf("starting state = %+v", state)
	}
	err := ExecViaSocket(f.paths.Socket, protocol.ExecRequest{Argv: []string{"true"}}, strings.NewReader(""), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("exec before ready: %v, want not ready", err)
	}
	reply := rawRequest(t, f.paths.Socket, `{"version":1,"argv":["true"]}`)
	if reply.OK || reply.Code != ReplyCodeNotReady || reply.Error != "not ready" {
		t.Fatalf("raw exec reply before ready = %+v", reply)
	}

	events, err := SubscribeEvents(f.paths.Socket, 10, true)
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	close(release)
	var seen []string
	for {
		e, err := events.Next()
		if err != nil {
			t.Fatalf("events: %v (seen %v)", err, seen)
		}
		if e.Type == EventState {
			seen = append(seen, e.State)
		}
		if e.Type == EventLog && e.Source == SourceDevcontainer && e.Message != "building image" {
			t.Fatalf("devcontainer log = %+v", e)
		}
		if e.State == StateReady {
			break
		}
	}
	if strings.Join(seen, ",") != "starting,connecting,ready" {
		t.Fatalf("states = %v", seen)
	}

	state, err = QueryInstance(f.paths.Socket)
	if err != nil || state.ContainerName != "fake-1" || state.WorkspaceFolder != "/workspaces/fake" || !state.EndpointConnected {
		t.Fatalf("ready state = %+v, %v", state, err)
	}
	var stdout strings.Builder
	err = ExecViaSocket(f.paths.Socket, protocol.ExecRequest{Argv: []string{"cat"}}, strings.NewReader("hi"), &stdout, io.Discard)
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 3 || stdout.String() != "hi" {
		t.Fatalf("exec when ready: %v, stdout %q", err, stdout.String())
	}
	if f.bridge.exec.requests[0].Cwd != "/workspaces/fake" {
		t.Fatalf("default cwd = %q", f.bridge.exec.requests[0].Cwd)
	}

	stopped, err := StopInstance(f.inst.opts.WorkspacePath, 5*time.Second)
	if !stopped || err != nil {
		t.Fatalf("StopInstance = %v, %v", stopped, err)
	}
	if err := f.wait(t); err != nil {
		t.Fatalf("run after stop = %v, want nil", err)
	}
	var last Event
	for {
		e, err := events.Next()
		if err != nil {
			break
		}
		if e.Type == EventState {
			last = e
		}
	}
	if last.State != StateStopped || last.Reason != ReasonStopRequested {
		t.Fatalf("final state event = %+v", last)
	}
	for _, path := range []string{f.paths.Socket, f.paths.State} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s remains after stop: %v", path, err)
		}
	}
}

func rawRequest(t *testing.T, socket, line string) ControlReply {
	t.Helper()
	var req ControlRequest
	if err := json.Unmarshal([]byte(line), &req); err != nil {
		t.Fatal(err)
	}
	conn, _, reply, err := controlRequest(socket, req, 5*time.Second)
	var replyErr *ReplyError
	if errors.As(err, &replyErr) {
		return ControlReply{ExecReply: protocol.ExecReply{Error: replyErr.Message, Code: replyErr.Code}}
	}
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	return *reply
}

func TestInstanceFailsWhenDevcontainerUpFails(t *testing.T) {
	f := startInstance(t, func(d *instanceDeps) {
		d.devcontainerUp = func(ctx context.Context, _, _ string, _ io.Writer) (*ContainerInfo, error) {
			time.Sleep(300 * time.Millisecond)
			return nil, errors.New("no devcontainer.json")
		}
	})
	events, err := SubscribeEvents(f.paths.Socket, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	err = f.wait(t)
	if err == nil || !strings.Contains(err.Error(), "no devcontainer.json") {
		t.Fatalf("run = %v", err)
	}
	var states []string
	for {
		e, err := events.Next()
		if err != nil {
			break
		}
		if e.Type == EventState {
			states = append(states, e.State+":"+e.Reason)
		}
	}
	if got := states[len(states)-1]; !strings.HasPrefix(got, "failed:failed to start devcontainer") {
		t.Fatalf("states = %v", states)
	}
}

func TestInstanceRejectsSecondInstance(t *testing.T) {
	f := startInstance(t, nil)
	waitState(t, f.paths.Socket, StateReady)
	second := newInstance(InstanceOptions{WorkspacePath: f.inst.opts.WorkspacePath}, f.bridge.deps())
	if err := second.run(context.Background()); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second instance = %v, want ErrAlreadyRunning", err)
	}
}

func TestSocketOps(t *testing.T) {
	f := startInstance(t, nil)
	waitState(t, f.paths.Socket, StateReady)
	for line, want := range map[string]string{
		`{"version":1,"op":"bogus"}`:                `unknown op "bogus"`,
		`{"version":1,"op":"exec","argv":[]}`:       "argv must not be empty",
		`{"version":1,"op":"events","history":501}`: "history must be between",
	} {
		if reply := rawRequest(t, f.paths.Socket, line); reply.OK || !strings.Contains(reply.Error, want) {
			t.Errorf("%s: reply = %+v, want %q", line, reply, want)
		}
	}
	// v0.7.0 clients send no op: still an exec.
	var stdout strings.Builder
	ExecViaSocket(f.paths.Socket, protocol.ExecRequest{Argv: []string{"cat"}}, strings.NewReader("legacy"), &stdout, io.Discard)
	if stdout.String() != "legacy" {
		t.Fatalf("op-less exec stdout = %q", stdout.String())
	}

	events, err := SubscribeEvents(f.paths.Socket, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for {
		e, err := events.Next()
		if err != nil {
			break
		}
		types = append(types, e.Type)
	}
	if strings.Join(types, ",") != "state,ports" {
		t.Fatalf("snapshot without follow = %v", types)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func TestInstanceReconnectsAfterBridgeLoss(t *testing.T) {
	var running atomic.Bool
	running.Store(true)
	port := freePort(t)
	f := startInstance(t, func(d *instanceDeps) {
		d.containerRunning = func(string) bool { return running.Load() }
		d.retryMin, d.retryMax, d.reconnectWindow = 20*time.Millisecond, 50*time.Millisecond, time.Minute
	})
	f.bridge.ports = []protocol.PortInfo{{Port: port, Address: "127.0.0.1"}}
	f.bridge.exec.ignoreEOF = true
	// The first bridge came up before the ports were set; reconnecting once
	// makes the endpoint report them.
	waitState(t, f.paths.Socket, StateReady)
	f.bridge.breakBridge()
	waitFor(t, "second connect", func() bool { return f.bridge.connects() == 2 })
	state := waitState(t, f.paths.Socket, StateReady)
	waitFor(t, "forwarded port", func() bool {
		s, _ := QueryInstance(f.paths.Socket)
		return s != nil && len(s.Ports) == 1 && s.Ports[0].Port == port
	})

	events, err := SubscribeEvents(f.paths.Socket, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	execDone := make(chan error, 1)
	var stderr syncBuffer
	go func() {
		execDone <- ExecViaSocket(f.paths.Socket, protocol.ExecRequest{Argv: []string{"sleep"}}, strings.NewReader(""), io.Discard, &stderr)
	}()
	waitFor(t, "exec running", func() bool {
		f.bridge.exec.mu.Lock()
		defer f.bridge.exec.mu.Unlock()
		return len(f.bridge.exec.requests) == 1
	})

	running.Store(false) // the container restarts
	f.bridge.breakBridge()
	select {
	case err := <-execDone:
		var exitErr *ExitError
		if !errors.As(err, &exitErr) || exitErr.Code != 255 || !strings.Contains(stderr.String(), "bridge lost") {
			t.Fatalf("exec after bridge loss = %v, stderr %q; want 255 and bridge lost", err, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("exec client was not told about the lost bridge")
	}
	waitState(t, f.paths.Socket, StateReconnecting)

	// Listeners stay open, new tunnel connections are refused.
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
	if err != nil {
		t.Fatalf("listener closed during reconnect: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if n, err := conn.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		t.Fatalf("tunnel during reconnect: read %d, %v; want EOF", n, err)
	}
	conn.Close()
	err = ExecViaSocket(f.paths.Socket, protocol.ExecRequest{Argv: []string{"true"}}, strings.NewReader(""), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("exec during reconnect = %v, want not ready", err)
	}

	running.Store(true)
	state = waitState(t, f.paths.Socket, StateReady)
	if state.Reconnects != 2 || !state.EndpointConnected || f.bridge.connects() != 3 {
		t.Fatalf("state after reconnect = %+v, connects %d", state, f.bridge.connects())
	}
	var states []string
	for len(states) < 3 { // snapshot, reconnecting, ready
		e, err := events.Next()
		if err != nil {
			t.Fatal(err)
		}
		if e.Type == EventState {
			states = append(states, e.State+":"+e.Reason)
		}
	}
	states = states[1:]
	if strings.Join(states, ",") != "reconnecting:bridge_lost,ready:" {
		t.Fatalf("state events = %v", states)
	}
	f.bridge.exec.ignoreEOF = false
	var stdout strings.Builder
	ExecViaSocket(f.paths.Socket, protocol.ExecRequest{Argv: []string{"cat"}}, strings.NewReader("again"), &stdout, io.Discard)
	if stdout.String() != "again" {
		t.Fatalf("exec after reconnect: stdout %q", stdout.String())
	}
}

func TestInstanceStopsWhenContainerStaysDown(t *testing.T) {
	var running atomic.Bool
	running.Store(true)
	f := startInstance(t, func(d *instanceDeps) {
		d.containerRunning = func(string) bool { return running.Load() }
		d.retryMin, d.retryMax, d.reconnectWindow = 20*time.Millisecond, 40*time.Millisecond, 300*time.Millisecond
	})
	waitState(t, f.paths.Socket, StateReady)
	events, err := SubscribeEvents(f.paths.Socket, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	running.Store(false)
	f.bridge.breakBridge()
	if err := f.wait(t); err != nil {
		t.Fatalf("run = %v, want a clean stop", err)
	}
	var last Event
	for {
		e, err := events.Next()
		if err != nil {
			break
		}
		if e.Type == EventState {
			last = e
		}
	}
	if last.State != StateStopped || last.Reason != ReasonContainerStopped {
		t.Fatalf("final state = %+v", last)
	}
}
