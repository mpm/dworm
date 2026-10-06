package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// DefaultEnsureTimeout bounds how long Ensure waits for an instance to become
// ready; `devcontainer up` may build images for minutes.
const DefaultEnsureTimeout = 15 * time.Minute

// InstanceCommand is the hidden subcommand that runs a detached instance.
const InstanceCommand = "__instance"

// exitAlreadyRunning is the exit code of an instance that lost the lock race.
const exitAlreadyRunning = 3

// socketWait bounds how long Ensure waits for a locked instance to answer.
const socketWait = 10 * time.Second

// ErrContainerNotRunning is returned by Ensure with NoStart when the
// workspace's container is not running.
var ErrContainerNotRunning = errors.New("the devcontainer is not running (--no-start)")

// EnsureOptions configure Ensure.
type EnsureOptions struct {
	WorkspacePath string
	Executable    string            // dworm binary for the instance; "" = this one
	SpawnArgs     []string          // global flags for a started instance (-c, -e, --bind)
	Env           map[string]string // -e values; a running instance with others gets a warning
	NoStart       bool              // fail instead of starting a stopped container
	Timeout       time.Duration     // 0 = DefaultEnsureTimeout
	Progress      io.Writer         // startup progress (log events); nil = quiet
	Warnings      io.Writer         // warnings; nil = discarded
	Version       string            // this client's version; instances must match
}

// errInstanceGone means the instance stopped while we waited for it.
var errInstanceGone = errors.New("instance stopped")

type ensurer struct {
	opts   EnsureOptions
	paths  InstancePaths
	spawns int
	warned bool
}

// Ensure makes sure the workspace's instance runs and is ready, starting a
// detached one if needed, and returns its live state.
func Ensure(ctx context.Context, opts EnsureOptions) (*InstanceState, error) {
	if opts.Timeout == 0 {
		opts.Timeout = DefaultEnsureTimeout
	}
	if opts.Warnings == nil {
		opts.Warnings = io.Discard
	}
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	e := &ensurer{opts: opts, paths: InstancePathsFor(opts.WorkspacePath)}
	for attempt := 0; ; attempt++ {
		state, err := e.attach(ctx)
		if err == nil {
			state, err = e.waitReady(ctx, state)
		}
		if errors.Is(err, errInstanceGone) && attempt < 3 {
			// Stopped meanwhile (`dworm stop`, container gone): start anew
			// once its lock is released.
			if err := e.waitUnlocked(ctx); err != nil {
				return nil, e.ctxError(ctx, err)
			}
			continue
		}
		if err != nil {
			return nil, e.ctxError(ctx, err)
		}
		return state, nil
	}
}

func (e *ensurer) ctxError(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("timed out after %v waiting for the dworm instance (see `dworm logs`)", e.opts.Timeout)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// attach returns the state of the running instance, starting one if needed.
func (e *ensurer) attach(ctx context.Context) (*InstanceState, error) {
	var child *spawnedInstance
	socketDeadline := time.Now().Add(socketWait)
	for {
		state, err := QueryInstance(e.paths.Socket)
		if err == nil {
			return state, e.check(state)
		}
		if errors.Is(err, ErrLegacyInstance) {
			return nil, fmt.Errorf("an older `dworm up` (v0.7.x) is running for this workspace; stop it first (`dworm stop`)")
		}
		running, lockErr := InstanceRunning(e.paths)
		if lockErr != nil {
			return nil, lockErr
		}
		if !running && (child == nil || child.exitedWith(exitAlreadyRunning)) {
			if e.spawns >= 3 {
				return nil, fmt.Errorf("could not start a dworm instance: %v", err)
			}
			if e.opts.NoStart {
				if _, err := GetContainerID(e.opts.WorkspacePath); err != nil {
					return nil, ErrContainerNotRunning
				}
			}
			child, err = e.spawn()
			if err != nil {
				return nil, err
			}
			socketDeadline = time.Now().Add(socketWait)
		}
		if child != nil {
			if code, exited := child.exitCode(); exited && code != exitAlreadyRunning {
				return nil, fmt.Errorf("dworm instance exited with code %d during startup%s", code, e.logTail())
			}
		}
		if time.Now().After(socketDeadline) {
			return nil, fmt.Errorf("dworm instance does not answer on %s: %v", e.paths.Socket, err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// check rejects instances of another dworm version and warns about -e values
// that do not apply.
func (e *ensurer) check(state *InstanceState) error {
	if state.DwormVersion != e.opts.Version {
		return fmt.Errorf("instance runs %s, this is %s; run `dworm stop`", state.DwormVersion, e.opts.Version)
	}
	if len(e.opts.Env) > 0 && !e.warned && EnvHash(e.opts.Env) != state.EnvHash {
		e.warned = true
		fmt.Fprintln(e.opts.Warnings, "Warning: the dworm instance is already running with different -e values; "+
			"they only apply when it starts. Run `dworm stop` first to apply them.")
	}
	return nil
}

// waitReady follows the instance's events until it is ready.
func (e *ensurer) waitReady(ctx context.Context, state *InstanceState) (*InstanceState, error) {
	switch state.State {
	case StateReady:
		return state, nil
	case StateFailed:
		return nil, e.failure(state.Reason, nil)
	case StateStopped:
		return nil, errInstanceGone
	}
	stream, err := SubscribeEvents(e.paths.Socket, 10, true)
	if err != nil {
		return nil, errInstanceGone
	}
	defer stream.Close()
	stop := context.AfterFunc(ctx, func() { stream.Close() })
	defer stop()

	var recent []string
	for {
		event, err := stream.Next()
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, errInstanceGone
		}
		switch event.Type {
		case EventLog:
			line := fmt.Sprintf("[%s] %s", event.Source, event.Message)
			recent = append(recent, line)
			if len(recent) > 10 {
				recent = recent[1:]
			}
			if e.opts.Progress != nil {
				fmt.Fprintln(e.opts.Progress, line)
			}
		case EventState:
			switch event.State {
			case StateReady:
				return QueryInstance(e.paths.Socket)
			case StateFailed:
				return nil, e.failure(event.Reason, recent)
			case StateStopped:
				return nil, errInstanceGone
			}
			if e.opts.Progress != nil {
				fmt.Fprintf(e.opts.Progress, "dworm instance: %s\n", event.State)
			}
		}
	}
}

func (e *ensurer) failure(reason string, recent []string) error {
	msg := "dworm instance failed: " + reason
	if len(recent) > 0 {
		msg += "\nLast log lines:\n  " + strings.Join(recent, "\n  ")
	} else {
		msg += e.logTail()
	}
	return errors.New(msg)
}

func (e *ensurer) waitUnlocked(ctx context.Context) error {
	deadline := time.Now().Add(socketWait)
	for {
		running, err := InstanceRunning(e.paths)
		if err != nil || !running {
			return err
		}
		if time.Now().After(deadline) {
			return errors.New("the stopped dworm instance did not exit")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// logTail returns the last lines of the detached instance log, formatted for
// an error message.
func (e *ensurer) logTail() string {
	data, err := os.ReadFile(e.paths.Log)
	if err != nil || len(data) == 0 {
		return ""
	}
	lines := lastLines(data, 10)
	return fmt.Sprintf("\nLast lines of %s:\n  %s", e.paths.Log, strings.Join(lines, "\n  "))
}

// spawnedInstance is a detached instance started by this client.
type spawnedInstance struct {
	exited chan struct{}
	code   int
}

func (s *spawnedInstance) exitCode() (int, bool) {
	select {
	case <-s.exited:
		return s.code, true
	default:
		return 0, false
	}
}

func (s *spawnedInstance) exitedWith(code int) bool {
	c, exited := s.exitCode()
	return exited && c == code
}

// spawn starts `dworm __instance` in a new session, with stdin from /dev/null
// and output appended to the instance log.
func (e *ensurer) spawn() (*spawnedInstance, error) {
	e.spawns++
	executable := e.opts.Executable
	if executable == "" {
		var err error
		if executable, err = os.Executable(); err != nil {
			return nil, err
		}
	}
	if err := ensureRuntimeDir(e.paths.Dir); err != nil {
		return nil, err
	}
	logFile, err := os.OpenFile(e.paths.Log, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return nil, fmt.Errorf("open instance log: %w", err)
	}
	defer logFile.Close()
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		return nil, err
	}
	defer devNull.Close()

	cmd := exec.Command(executable, append([]string{InstanceCommand}, e.opts.SpawnArgs...)...)
	cmd.Dir = e.opts.WorkspacePath
	cmd.Stdin = devNull
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = detachedAttrs()
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start dworm instance: %w", err)
	}
	child := &spawnedInstance{exited: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			child.code = exitErr.ExitCode()
		} else if err != nil {
			child.code = -1
		}
		close(child.exited)
	}()
	return child, nil
}

// lastLines returns up to n trailing lines of data.
func lastLines(data []byte, n int) []string {
	lines := strings.Split(string(bytes.TrimRight(data, "\n")), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}
