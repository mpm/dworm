package host

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/mpm/dworm/internal/config"
	"github.com/mpm/dworm/internal/protocol"
)

// Instance modes (InstanceState.Mode).
const (
	ModeDetached   = "detached"   // started by a client (`dworm __instance`)
	ModeForeground = "foreground" // `dworm up --foreground`
)

// Reasons of the "stopped" state.
const (
	ReasonStopRequested    = "stop_requested"
	ReasonSignal           = "signal"
	ReasonContainerStopped = "container_stopped"
)

// InstanceOptions configure RunInstance.
type InstanceOptions struct {
	WorkspacePath string
	ConfigPath    string            // devcontainer.json; "" = default lookup
	BindAddr      string            // "" = .dworm.config bind, else 127.0.0.1
	Env           map[string]string // CLI -e values
	Mode          string
	Version       string
	Output        io.Writer // log destination
	// OpenOutput, when set, replaces Output once the instance holds the
	// workspace lock (so a losing instance never touches the log file).
	OpenOutput func() (io.Writer, error)
	LogPath    string // reported in the status when the output is a file
}

// endpointConn is a started endpoint process with its bridge multiplexer.
type endpointConn interface {
	Mux() *protocol.Mux
	Close() error
}

// instanceDeps are the side effects of an instance, replaced in tests.
type instanceDeps struct {
	devcontainerUp   func(ctx context.Context, workspacePath, configPath string, progress io.Writer) (*ContainerInfo, error)
	startEndpoint    func(ctx context.Context, containerID string, hostLog, endpointStderr io.Writer) (endpointConn, error)
	containerRunning func(containerID string) bool
	forwarding       func(*log.Logger) forwardingConfig
	initTimeout      time.Duration
	// Reconnect backoff while the container is down, and how long to keep
	// trying before the instance stops.
	retryMin, retryMax time.Duration
	reconnectWindow    time.Duration
}

func defaultInstanceDeps() instanceDeps {
	return instanceDeps{
		devcontainerUp: DevcontainerUp,
		startEndpoint: func(ctx context.Context, containerID string, hostLog, endpointStderr io.Writer) (endpointConn, error) {
			e := NewEndpointManager(containerID, hostLog)
			e.logger = log.New(hostLog, "[host] ", 0) // the instance log adds timestamps
			e.stderrWriter = endpointStderr
			if err := e.InjectAndStart(); err != nil {
				e.Close()
				return nil, err
			}
			return e, nil
		},
		containerRunning: IsContainerRunning,
		forwarding:       detectForwarding,
		initTimeout:      30 * time.Second,
		retryMin:         time.Second,
		retryMax:         30 * time.Second,
		reconnectWindow:  2 * time.Minute,
	}
}

// Instance owns the bridge of one workspace: the lock, the state file, the
// container, the endpoint, port forwarding, agent forwarding, and the socket
// that clients (`dworm shell`, `dworm exec`, other programs) talk to.
type Instance struct {
	opts InstanceOptions
	deps instanceDeps

	bus             *EventBus
	hostLog         *eventLogWriter
	endpointStderr  *eventLogWriter
	devcontainerLog *eventLogWriter
	logger          *log.Logger

	state      *StateFile
	server     *ExecServer
	tunnels    *TunnelManager
	forwarding forwardingConfig
	cancel     context.CancelFunc
	background sync.WaitGroup

	mu         sync.Mutex
	stopReason string
	bridge     *bridgeSession
	env        map[string]string
}

// bridgeSession is one connected endpoint.
type bridgeSession struct {
	conn  endpointConn
	mux   *protocol.Mux
	agent *AgentHandler
	done  chan struct{} // closed when the control stream fails
	err   error
}

func (b *bridgeSession) close() {
	if b.agent != nil {
		b.agent.Close()
	}
	b.conn.Close()
}

// RunInstance runs an instance until it is stopped (op stop or ctx, e.g. on
// SIGTERM). It returns ErrAlreadyRunning when another instance holds the
// workspace lock, nil after a clean stop, and an error when the instance
// failed.
func RunInstance(ctx context.Context, opts InstanceOptions) error {
	return newInstance(opts, defaultInstanceDeps()).run(ctx)
}

func newInstance(opts InstanceOptions, deps instanceDeps) *Instance {
	i := &Instance{opts: opts, deps: deps, bus: NewEventBus()}
	i.setOutput(opts.Output)
	return i
}

func (i *Instance) setOutput(out io.Writer) {
	if out == nil {
		out = io.Discard
	}
	i.hostLog = newEventLogWriter(i.bus, out, SourceHost, "")
	i.endpointStderr = newEventLogWriter(i.bus, out, SourceEndpoint, "")
	i.devcontainerLog = newEventLogWriter(i.bus, out, SourceDevcontainer, "[devcontainer] ")
	i.logger = log.New(i.hostLog, "", 0)
}

// EnvHash identifies a set of CLI -e values ("" when empty).
func EnvHash(env map[string]string) string {
	if len(env) == 0 {
		return ""
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		fmt.Fprintf(h, "%s=%s\x00", k, env[k])
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func (i *Instance) run(parent context.Context) error {
	lock, err := AcquireInstanceLock(i.opts.WorkspacePath)
	if err != nil {
		return err
	}
	defer lock.Release()
	if i.opts.OpenOutput != nil {
		out, err := i.opts.OpenOutput()
		if err != nil {
			return err
		}
		i.setOutput(out)
	}
	i.state, err = NewStateFile(lock.Paths.State, InstanceState{
		PID:           os.Getpid(),
		WorkspacePath: i.opts.WorkspacePath,
		StartedAt:     time.Now().UTC(),
		State:         StateStarting,
		Mode:          i.opts.Mode,
		LogPath:       i.opts.LogPath,
		DwormVersion:  i.opts.Version,
		EnvHash:       EnvHash(i.opts.Env),
	})
	if err != nil {
		return fmt.Errorf("write state file: %w", err)
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	i.cancel = cancel
	i.bus.Publish(Event{Type: EventState, State: StateStarting})

	// The socket comes first so clients can follow the startup.
	i.server, err = NewExecServer(lock.Paths.Socket, ExecServerConfig{
		Open:            i.openStream,
		WorkspaceFolder: func() string { return i.state.Snapshot().WorkspaceFolder },
		Logger:          i.logger,
		Ready:           i.ready,
		Status:          i.state.Snapshot,
		Events:          i.bus,
		Stop:            func() { i.requestStop(ReasonStopRequested) },
	})
	if err != nil {
		i.setState(StateFailed, err.Error())
		return err
	}
	i.update(func(s *InstanceState) { s.ExecSocket = i.server.Path() })
	i.logger.Printf("Instance started for %s (pid %d, %s)", i.opts.WorkspacePath, os.Getpid(), i.opts.Mode)

	runErr := i.serve(ctx)
	if runErr == nil && i.reason() == "" && parent.Err() != nil {
		i.requestStop(ReasonSignal)
	}
	i.shutdown(runErr)
	return runErr
}

// serve starts the container and the bridge and returns when the instance
// should stop: nil for a requested stop, an error when it failed.
func (i *Instance) serve(ctx context.Context) error {
	ws := i.opts.WorkspacePath
	projectConfig, staticEnv, err := config.Load(ws)
	if err != nil {
		return err
	}
	bind := i.opts.BindAddr
	if bind == "" {
		bind = projectConfig.Bind
	}
	if err := config.Validate(i.opts.Env); err != nil {
		return err
	}
	generated, err := RunEnvironmentCommand(ctx, ws, projectConfig.HostEnv, i.hostLog)
	if ctx.Err() != nil {
		return nil
	}
	if err != nil {
		return err
	}
	i.setEnv(config.Merge(staticEnv, generated, i.opts.Env))

	i.logger.Printf("Starting devcontainer at %s...", ws)
	info, err := i.deps.devcontainerUp(ctx, ws, i.opts.ConfigPath, i.devcontainerLog)
	if ctx.Err() != nil {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to start devcontainer: %w", err)
	}
	i.logger.Printf("Container started: %s", info.ContainerName)
	i.update(func(s *InstanceState) {
		s.ContainerID = info.ContainerID
		s.ContainerName = info.ContainerName
		s.RemoteUser = info.RemoteUser
		s.WorkspaceFolder = info.WorkspaceDir
	})

	i.setState(StateConnecting, "")
	i.forwarding = i.deps.forwarding(i.logger)
	i.tunnels = NewTunnelManager(i, bind, log.New(i.hostLog, "[tunnel] ", 0))
	portCh := make(chan []PortMapping, 10)
	i.tunnels.SetPortUpdateChannel(portCh)
	i.goBackground(func() { i.watchPorts(ctx, portCh, i.tunnels.bindAddr) })

	session, err := i.connect(info.ContainerID)
	if ctx.Err() != nil {
		if session != nil {
			session.close()
		}
		return nil
	}
	if err != nil {
		return err
	}
	i.setBridge(session)
	i.setState(StateReady, "")

	interval, _, _ := projectConfig.HostEnv.Durations()
	i.goBackground(func() {
		RefreshEnvironment(ctx, interval, func() error {
			generated, err := RunEnvironmentCommand(ctx, ws, projectConfig.HostEnv, i.hostLog)
			if err != nil {
				return err
			}
			return i.sendEnvironment(config.Merge(staticEnv, generated, i.opts.Env))
		}, func(err error) { i.logger.Printf("Environment refresh failed (keeping previous values): %v", err) })
	})

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-session.done:
		}
		// systemd signals the whole cgroup, so the docker exec carrying the
		// bridge may die just before our own SIGTERM is delivered. A stop that
		// arrives with the failure is still a clean shutdown.
		if stopRequested(ctx, 500*time.Millisecond) {
			return nil
		}
		i.logger.Printf("Bridge lost: %v", session.err)
		if old := i.setBridge(nil); old != nil {
			old.close()
		}
		i.update(func(s *InstanceState) { s.Reconnects++ })
		i.setState(StateReconnecting, "bridge_lost")

		session, err = i.reconnect(ctx, info.ContainerID)
		if ctx.Err() != nil {
			if session != nil {
				session.close()
			}
			return nil
		}
		if errors.Is(err, errContainerStopped) {
			i.logger.Printf("Container stayed down for %v; stopping", i.deps.reconnectWindow)
			i.requestStop(ReasonContainerStopped)
			return nil
		}
		if err != nil {
			return err
		}
		i.setBridge(session)
		i.setState(StateReady, "")
	}
}

var errContainerStopped = errors.New("container is not running")

// reconnect re-injects the endpoint until it is connected. While the
// container is down it retries with backoff for the reconnect window and
// then returns errContainerStopped.
func (i *Instance) reconnect(ctx context.Context, containerID string) (*bridgeSession, error) {
	deadline := time.Now().Add(i.deps.reconnectWindow)
	delay := i.deps.retryMin
	for {
		var err error
		if i.deps.containerRunning(containerID) {
			var session *bridgeSession
			if session, err = i.connect(containerID); err == nil {
				i.logger.Printf("Bridge reconnected")
				return session, nil
			}
			i.logger.Printf("Reconnect failed: %v", err)
		} else {
			err = errContainerStopped
		}
		if time.Now().Add(delay).After(deadline) {
			if errors.Is(err, errContainerStopped) {
				return nil, err
			}
			return nil, fmt.Errorf("reconnect to the endpoint: %w", err)
		}
		if errors.Is(err, errContainerStopped) {
			i.logger.Printf("Container is not running; retrying in %v", delay)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
		delay *= 2
		if delay > i.deps.retryMax {
			delay = i.deps.retryMax
		}
	}
}

// stopRequested reports whether ctx ends within grace.
func stopRequested(ctx context.Context, grace time.Duration) bool {
	select {
	case <-ctx.Done():
		return true
	case <-time.After(grace):
		return false
	}
}

func (i *Instance) shutdown(runErr error) {
	i.setState(StateStopping, i.reason())
	if runErr == nil {
		i.logger.Printf("Shutting down (%s)...", i.reason())
	} else {
		i.logger.Printf("Shutting down...")
	}
	i.server.CloseExecs()
	if i.tunnels != nil {
		i.tunnels.Close()
	}
	if b := i.setBridge(nil); b != nil {
		b.close()
	}
	i.cancel()
	waitTimeout(&i.background, 2*time.Second)
	if runErr != nil {
		i.logger.Printf("Instance failed: %v", runErr)
		i.setState(StateFailed, runErr.Error())
	} else {
		i.setState(StateStopped, i.reason())
	}
	i.bus.Close()
	i.server.Close()
}

func waitTimeout(wg *sync.WaitGroup, timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

func (i *Instance) goBackground(f func()) {
	i.background.Add(1)
	go func() {
		defer i.background.Done()
		f()
	}()
}

// connect starts an endpoint, initializes it, and starts reading its control
// messages.
func (i *Instance) connect(containerID string) (*bridgeSession, error) {
	conn, err := i.deps.startEndpoint(context.Background(), containerID, i.hostLog, i.endpointStderr)
	if err != nil {
		return nil, fmt.Errorf("failed to start endpoint: %w", err)
	}
	mux := conn.Mux()
	env := i.currentEnv()
	if err := mux.SendControl(protocol.TypeInit, i.forwarding.initMessage(env)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to send init: %w", err)
	}
	if err := waitEnvironmentReady(mux, i.deps.initTimeout); err != nil {
		conn.Close()
		return nil, fmt.Errorf("initialize environment: %w", err)
	}
	session := &bridgeSession{conn: conn, mux: mux, done: make(chan struct{})}
	if i.forwarding.any() {
		session.agent = NewAgentHandler(mux, i.forwarding.sshSocket, i.logger)
		if i.forwarding.gpgSocket != "" {
			session.agent.SetGPGSocketPath(i.forwarding.gpgSocket)
		}
		session.agent.Start()
	}
	if i.forwarding.sshSocket != "" {
		i.logger.Printf("SSH agent forwarding enabled")
	}
	if i.forwarding.gpgSocket != "" {
		i.logger.Printf("GPG agent forwarding enabled")
	}
	if i.forwarding.gitCred {
		i.logger.Printf("Git credential forwarding enabled")
	}
	i.logger.Printf("Endpoint initialized with %d env vars", len(env))
	go i.readControl(session)
	return session, nil
}

// readControl handles control messages until the bridge fails.
func (i *Instance) readControl(session *bridgeSession) {
	for {
		msgType, data, err := session.mux.RecvControl()
		if err != nil {
			session.err = err
			close(session.done)
			return
		}
		switch msgType {
		case protocol.TypePortUpdate:
			portMsg, err := protocol.DecodePortUpdate(data)
			if err != nil {
				i.logger.Printf("Failed to decode port update: %v", err)
				continue
			}
			i.tunnels.UpdatePorts(portMsg.Ports)
		}
	}
}

// watchPorts mirrors forwarded ports into the state and the event stream.
func (i *Instance) watchPorts(ctx context.Context, ch <-chan []PortMapping, bind string) {
	for {
		select {
		case <-ctx.Done():
			return
		case ports := <-ch:
			statePorts := make([]StatePort, len(ports))
			for n, p := range ports {
				statePorts[n] = StatePort{Port: p.ContainerPort, Address: bind, LocalPort: p.LocalPort}
			}
			sort.Slice(statePorts, func(a, b int) bool { return statePorts[a].Port < statePorts[b].Port })
			i.update(func(s *InstanceState) { s.Ports = statePorts })
			i.bus.Publish(Event{Type: EventPorts, Ports: statePorts})
		}
	}
}

func (i *Instance) setEnv(env map[string]string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.env = env
}

func (i *Instance) currentEnv() map[string]string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return config.Merge(i.env)
}

// sendEnvironment stores env and publishes it to the connected endpoint.
func (i *Instance) sendEnvironment(env map[string]string) error {
	i.setEnv(env)
	b := i.currentBridge()
	if b == nil {
		return nil // sent with the next init
	}
	next := config.Merge(env)
	if i.forwarding.sshSocket != "" {
		next["SSH_AUTH_SOCK"] = protocol.SSHAgentSocketPath
	}
	return b.mux.SendControl(protocol.TypeEnvironment, next)
}

// setBridge replaces the current bridge and returns the previous one.
func (i *Instance) setBridge(b *bridgeSession) *bridgeSession {
	i.mu.Lock()
	old := i.bridge
	i.bridge = b
	i.mu.Unlock()
	i.update(func(s *InstanceState) { s.EndpointConnected = b != nil })
	return old
}

func (i *Instance) currentBridge() *bridgeSession {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.bridge
}

func (i *Instance) ready() bool {
	return i.readyBridge() != nil
}

// readyBridge returns the bridge while the instance is ready.
func (i *Instance) readyBridge() *bridgeSession {
	b := i.currentBridge()
	if b == nil || i.state.Snapshot().State != StateReady {
		return nil
	}
	return b
}

var errBridgeNotReady = errors.New("bridge is not ready")

func (i *Instance) openStream() (net.Conn, error) {
	b := i.readyBridge()
	if b == nil {
		return nil, errBridgeNotReady
	}
	return b.mux.OpenStream()
}

// OpenTunnelStream implements tunnelOpener: new tunnel connections are
// refused while the bridge is not ready.
func (i *Instance) OpenTunnelStream(port int) (net.Conn, error) {
	b := i.readyBridge()
	if b == nil {
		return nil, errBridgeNotReady
	}
	return openTunnelStream(b.mux, port, 0)
}

func (i *Instance) update(change func(*InstanceState)) {
	if err := i.state.Update(change); err != nil {
		i.logger.Printf("Warning: failed to update state file: %v", err)
	}
}

func (i *Instance) setState(state, reason string) {
	i.update(func(s *InstanceState) {
		s.State = state
		s.Reason = reason
	})
	i.bus.Publish(Event{Type: EventState, State: state, Reason: reason})
}

// requestStop stops the instance; the first reason wins.
func (i *Instance) requestStop(reason string) {
	i.mu.Lock()
	if i.stopReason == "" {
		i.stopReason = reason
	}
	i.mu.Unlock()
	if i.cancel != nil {
		i.cancel()
	}
}

func (i *Instance) reason() string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.stopReason
}
