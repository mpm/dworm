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

	"github.com/creack/pty"
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
	tty  bool // a TTY session: pgid is also its session ID
	pgid int  // 0 until started; guarded by execManager.mu
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
	var procs []execProcess
	for p := range m.running {
		if p.pgid > 0 {
			procs = append(procs, *p)
		}
	}
	m.mu.Unlock()

	var wg sync.WaitGroup
	for _, p := range procs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.kill(&p)
		}()
	}
	wg.Wait()
}

// kill ends a process whose caller is gone: a TTY session is hung up, a
// plain process group terminated.
func (m *execManager) kill(p *execProcess) {
	if p.tty {
		m.hangup(p.pgid)
	} else {
		m.terminate(p.pgid)
	}
}

// hangup ends a TTY session like a closed terminal: SIGHUP to every member
// first, then SIGTERM and, after the grace period, SIGKILL.
func (m *execManager) hangup(sid int) {
	if sid <= 0 {
		return
	}
	hupGrace := min(m.grace, time.Second)
	for _, step := range []struct {
		sig   syscall.Signal
		grace time.Duration
	}{{syscall.SIGHUP, hupGrace}, {syscall.SIGTERM, m.grace}, {syscall.SIGKILL, 0}} {
		signalSession(sid, step.sig)
		deadline := time.Now().Add(step.grace)
		for time.Now().Before(deadline) {
			if !sessionAlive(sid) {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
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

	p.tty = req.TTY != nil
	var cmd *exec.Cmd
	var stdio *processStdio
	if p.tty {
		cmd, stdio, err = startTTYProcess(req.Argv, req.Cwd, env, req.TTY)
	} else {
		cmd, stdio, err = startProcess(req.Argv, req.Cwd, env)
	}
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
		m.kill(p)
	}

	callerGone := make(chan struct{})
	var goneOnce sync.Once
	markGone := func() { goneOnce.Do(func() { close(callerGone) }) }

	if err := protocol.WriteJSONMessage(stream, protocol.ExecReply{OK: true, ID: req.ID}); err != nil {
		markGone()
	}
	stream.SetDeadline(time.Time{})
	mode := ""
	if p.tty {
		mode = ", tty"
	}
	m.logger.Printf("Exec %s started: %s (pid %d%s)", req.ID, req.Argv[0], pgid, mode)

	var writeMu sync.Mutex
	writeFrame := func(frameType byte, payload []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return protocol.WriteFrame(stream, frameType, payload)
	}

	var pumps sync.WaitGroup
	pump := func(r *os.File, frameType byte) {
		defer pumps.Done()
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
	if p.tty {
		pumps.Add(1)
		go pump(stdio.pty, protocol.FrameStdout)
	} else {
		pumps.Add(2)
		go func() { pump(stdio.stdout, protocol.FrameStdout); stdio.stdout.Close() }()
		go func() { pump(stdio.stderr, protocol.FrameStderr); stdio.stderr.Close() }()
	}
	pumpsDone := make(chan struct{})
	go func() {
		pumps.Wait()
		close(pumpsDone)
	}()

	stdinCh := make(chan []byte, 16)
	go func() {
		if !p.tty {
			defer stdio.stdin.Close()
		}
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
				// A terminal has no EOF; keys like Ctrl-D are stdin bytes.
				if !p.tty {
					closeStdin()
				}
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
			case protocol.FrameResize:
				var size protocol.ExecResize
				if p.tty && json.Unmarshal(payload, &size) == nil {
					pty.Setsize(stdio.pty, winsize(size.Rows, size.Cols))
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
		m.kill(p)
		<-waitErr
	}
	if p.tty {
		// Like a closing terminal: output still buffered in the PTY is
		// forwarded briefly, then the remaining session members get SIGHUP.
		select {
		case <-pumpsDone:
		case <-callerGone:
		case <-time.After(time.Second):
		}
		stdio.pty.Close()
		<-pumpsDone
		signalSession(pgid, syscall.SIGHUP)
	} else {
		// The process has exited; drain output still held by its group.
		select {
		case <-pumpsDone:
		case <-callerGone:
			m.terminate(pgid)
			<-pumpsDone
		}
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
	stdin, stdout, stderr *os.File // parent ends (TTY mode: stdin is pty)
	pty                   *os.File // PTY master in TTY mode
}

func winsize(rows, cols int) *pty.Winsize {
	if rows <= 0 || cols <= 0 {
		rows, cols = 24, 80
	}
	return &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)}
}

// startTTYProcess starts argv on a new PTY, as session leader with the PTY
// as its controlling terminal (so its process group ID is also the session
// ID).
func startTTYProcess(argv []string, dir string, env map[string]string, tty *protocol.ExecTTY) (*exec.Cmd, *processStdio, error) {
	path, err := lookPathIn(argv[0], env["PATH"], dir)
	if err != nil {
		return nil, nil, err
	}
	if tty.Term != "" {
		env = config.Merge(env, map[string]string{"TERM": tty.Term})
	}
	environ := make([]string, 0, len(env))
	for k, v := range env {
		environ = append(environ, k+"="+v)
	}
	cmd := &exec.Cmd{Path: path, Args: argv, Env: environ, Dir: dir}
	ptmx, err := pty.StartWithAttrs(cmd, winsize(tty.Rows, tty.Cols), &syscall.SysProcAttr{Setsid: true, Setctty: true})
	if err != nil {
		return nil, nil, err
	}
	return cmd, &processStdio{stdin: ptmx, pty: ptmx}, nil
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
