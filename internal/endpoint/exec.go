package endpoint

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mpm/dworm/internal/config"
	"github.com/mpm/dworm/internal/protocol"
)

// execManager runs processes requested over exec streams. Each process gets
// its own process group, which is terminated when the caller disconnects
// before the process exits, or when the endpoint shuts down.
type execManager struct {
	logger        *log.Logger
	limit         int
	grace         time.Duration
	headerTimeout time.Duration

	mu      sync.Mutex
	closed  bool
	running map[*execProcess]struct{}
}

type execProcess struct {
	id   string
	pgid int // 0 until started; guarded by execManager.mu
}

func newExecManager(logger *log.Logger) *execManager {
	return &execManager{
		logger:        logger,
		limit:         protocol.MaxConcurrentExecs,
		grace:         protocol.ExecKillGrace,
		headerTimeout: 10 * time.Second,
		running:       make(map[*execProcess]struct{}),
	}
}

func (m *execManager) register(p *execProcess) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errors.New("endpoint is shutting down")
	}
	if len(m.running) >= m.limit {
		return fmt.Errorf("too many concurrent execs (limit %d)", m.limit)
	}
	m.running[p] = struct{}{}
	return nil
}

func (m *execManager) unregister(p *execProcess) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.running, p)
}

// started records the process group and reports whether the manager closed
// in the meantime (the caller must then terminate the group itself).
func (m *execManager) started(p *execProcess, pgid int) (closed bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p.pgid = pgid
	return m.closed
}

// closeAll terminates every running process group and waits for them.
func (m *execManager) closeAll() {
	m.mu.Lock()
	m.closed = true
	var groups []int
	for p := range m.running {
		if p.pgid > 0 {
			groups = append(groups, p.pgid)
		}
	}
	m.mu.Unlock()

	var wg sync.WaitGroup
	for _, pgid := range groups {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.terminate(pgid)
		}()
	}
	wg.Wait()
}

// terminate sends SIGTERM to the process group and SIGKILL after the grace
// period if any member is still alive.
func (m *execManager) terminate(pgid int) {
	if pgid <= 0 {
		return
	}
	syscall.Kill(-pgid, syscall.SIGTERM)
	deadline := time.Now().Add(m.grace)
	for time.Now().Before(deadline) {
		if !groupAlive(pgid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	syscall.Kill(-pgid, syscall.SIGKILL)
}

func groupAlive(pgid int) bool {
	return syscall.Kill(-pgid, 0) != syscall.ESRCH
}

func rejectExec(stream net.Conn, reply protocol.ExecReply) {
	reply.OK = false
	protocol.WriteJSONMessage(stream, reply)
}

// handle serves one exec stream after its type marker has been read.
func (m *execManager) handle(stream net.Conn) {
	defer stream.Close()
	stream.SetDeadline(time.Now().Add(m.headerTimeout))

	var req protocol.ExecRequest
	if err := protocol.ReadJSONMessage(stream, protocol.MaxExecHeaderSize, &req); err != nil {
		rejectExec(stream, protocol.ExecReply{Error: fmt.Sprintf("invalid exec request: %v", err)})
		return
	}
	if len(req.Argv) == 0 {
		rejectExec(stream, protocol.ExecReply{ID: req.ID, Error: "argv must not be empty"})
		return
	}
	base, published, err := launchLayers(req.Env)
	if err != nil {
		rejectExec(stream, protocol.ExecReply{ID: req.ID, Error: fmt.Sprintf("environment: %v", err)})
		return
	}
	env := config.Merge(base, published, req.Env)

	p := &execProcess{id: req.ID}
	if err := m.register(p); err != nil {
		rejectExec(stream, protocol.ExecReply{ID: req.ID, Error: err.Error()})
		return
	}
	defer m.unregister(p)

	cmd, stdio, err := startProcess(req.Argv, req.Cwd, env)
	if err != nil {
		code := 126
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, exec.ErrNotFound) {
			code = 127
		}
		rejectExec(stream, protocol.ExecReply{ID: req.ID, Error: err.Error(), ExitCode: code})
		return
	}
	pgid := cmd.Process.Pid
	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()
	if m.started(p, pgid) {
		m.terminate(pgid)
	}

	callerGone := make(chan struct{})
	var goneOnce sync.Once
	markGone := func() { goneOnce.Do(func() { close(callerGone) }) }

	if err := protocol.WriteJSONMessage(stream, protocol.ExecReply{OK: true, ID: req.ID}); err != nil {
		markGone()
	}
	stream.SetDeadline(time.Time{})
	m.logger.Printf("Exec %s started: %s (pid %d)", req.ID, req.Argv[0], pgid)

	var writeMu sync.Mutex
	writeFrame := func(frameType byte, payload []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return protocol.WriteFrame(stream, frameType, payload)
	}

	var pumps sync.WaitGroup
	pump := func(r *os.File, frameType byte) {
		defer pumps.Done()
		defer r.Close()
		buf := make([]byte, 32*1024)
		failed := false
		for {
			n, err := r.Read(buf)
			if n > 0 && !failed {
				if writeFrame(frameType, buf[:n]) != nil {
					// Keep draining so the process never blocks on a full pipe.
					failed = true
					markGone()
				}
			}
			if err != nil {
				return
			}
		}
	}
	pumps.Add(2)
	go pump(stdio.stdout, protocol.FrameStdout)
	go pump(stdio.stderr, protocol.FrameStderr)
	pumpsDone := make(chan struct{})
	go func() {
		pumps.Wait()
		close(pumpsDone)
	}()

	stdinCh := make(chan []byte, 16)
	go func() {
		defer stdio.stdin.Close()
		for data := range stdinCh {
			if _, err := stdio.stdin.Write(data); err != nil {
				for range stdinCh {
				}
				return
			}
		}
	}()

	go func() {
		stdinOpen := true
		closeStdin := func() {
			if stdinOpen {
				close(stdinCh)
				stdinOpen = false
			}
		}
		defer closeStdin()
		for {
			frameType, payload, err := protocol.ReadFrame(stream)
			if err != nil {
				// Stream closed or reset: the caller is gone. Stdin EOF is
				// an explicit frame, so this is never a graceful close.
				markGone()
				return
			}
			switch frameType {
			case protocol.FrameStdin:
				if stdinOpen {
					stdinCh <- payload
				}
			case protocol.FrameStdinEOF:
				closeStdin()
			case protocol.FrameSignal:
				var msg protocol.ExecSignal
				if json.Unmarshal(payload, &msg) != nil {
					continue
				}
				if sig, ok := signalByName(msg.Signal); ok {
					syscall.Kill(-pgid, sig)
				} else {
					m.logger.Printf("Exec %s: ignoring unknown signal %q", req.ID, msg.Signal)
				}
			default:
				m.logger.Printf("Exec %s: ignoring unknown frame type %d", req.ID, frameType)
			}
		}
	}()

	select {
	case <-waitErr:
	case <-callerGone:
		m.logger.Printf("Exec %s: caller disconnected, terminating process group %d", req.ID, pgid)
		m.terminate(pgid)
		<-waitErr
	}
	// The process has exited; drain output still held by its group.
	select {
	case <-pumpsDone:
	case <-callerGone:
		m.terminate(pgid)
		<-pumpsDone
	}

	exit := exitStatus(cmd.ProcessState)
	writeFrame(protocol.FrameExit, mustJSON(exit))
	if exit.Signal != "" {
		m.logger.Printf("Exec %s exited: signal %s", req.ID, exit.Signal)
	} else {
		m.logger.Printf("Exec %s exited: code %d", req.ID, exit.Code)
	}
}

type processStdio struct {
	stdin, stdout, stderr *os.File // parent ends
}

// startProcess starts argv in its own process group. argv[0] is resolved
// with the PATH of env, not of the endpoint.
func startProcess(argv []string, dir string, env map[string]string) (*exec.Cmd, *processStdio, error) {
	path, err := lookPathIn(argv[0], env["PATH"], dir)
	if err != nil {
		return nil, nil, err
	}
	environ := make([]string, 0, len(env))
	for k, v := range env {
		environ = append(environ, k+"="+v)
	}

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		stdinR.Close()
		stdinW.Close()
		return nil, nil, err
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		stdinR.Close()
		stdinW.Close()
		stdoutR.Close()
		stdoutW.Close()
		return nil, nil, err
	}
	cmd := &exec.Cmd{
		Path:        path,
		Args:        argv,
		Env:         environ,
		Dir:         dir,
		Stdin:       stdinR,
		Stdout:      stdoutW,
		Stderr:      stderrW,
		SysProcAttr: &syscall.SysProcAttr{Setpgid: true},
	}
	err = cmd.Start()
	// The child holds its own copies of these ends.
	stdinR.Close()
	stdoutW.Close()
	stderrW.Close()
	if err != nil {
		stdinW.Close()
		stdoutR.Close()
		stderrR.Close()
		return nil, nil, err
	}
	return cmd, &processStdio{stdin: stdinW, stdout: stdoutR, stderr: stderrR}, nil
}

func lookPathIn(file, pathEnv, dir string) (string, error) {
	if strings.Contains(file, "/") {
		path := file
		if !filepath.IsAbs(path) && dir != "" {
			path = filepath.Join(dir, path)
		}
		return file, checkExecutable(path)
	}
	for _, entry := range filepath.SplitList(pathEnv) {
		if entry == "" {
			entry = "."
		}
		path := filepath.Join(entry, file)
		if !filepath.IsAbs(path) && dir != "" {
			path = filepath.Join(dir, path)
		}
		if checkExecutable(path) == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("%s: %w in PATH", file, exec.ErrNotFound)
}

func checkExecutable(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() || info.Mode()&0111 == 0 {
		return fmt.Errorf("%s: %w", path, fs.ErrPermission)
	}
	return nil
}

func exitStatus(state *os.ProcessState) protocol.ExecExit {
	if state == nil {
		return protocol.ExecExit{Code: 255}
	}
	if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return protocol.ExecExit{Code: 128 + int(ws.Signal()), Signal: signalName(ws.Signal())}
	}
	return protocol.ExecExit{Code: state.ExitCode()}
}

var execSignals = map[string]syscall.Signal{
	"HUP":   syscall.SIGHUP,
	"INT":   syscall.SIGINT,
	"QUIT":  syscall.SIGQUIT,
	"KILL":  syscall.SIGKILL,
	"USR1":  syscall.SIGUSR1,
	"USR2":  syscall.SIGUSR2,
	"PIPE":  syscall.SIGPIPE,
	"ALRM":  syscall.SIGALRM,
	"TERM":  syscall.SIGTERM,
	"CONT":  syscall.SIGCONT,
	"STOP":  syscall.SIGSTOP,
	"TSTP":  syscall.SIGTSTP,
	"WINCH": syscall.SIGWINCH,
}

func signalByName(name string) (syscall.Signal, bool) {
	sig, ok := execSignals[strings.TrimPrefix(strings.ToUpper(name), "SIG")]
	return sig, ok
}

func signalName(sig syscall.Signal) string {
	for name, s := range execSignals {
		if s == sig {
			return name
		}
	}
	return fmt.Sprintf("%d", int(sig))
}

func mustJSON(v interface{}) []byte {
	data, _ := json.Marshal(v)
	return data
}
