package host

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mpm/dworm/internal/protocol"
	"golang.org/x/term"
)

// ErrExecSocketUnavailable means the instance socket could not be used and
// the command was not started.
var ErrExecSocketUnavailable = errors.New("dworm instance socket unavailable")

// ExecRejectedError is an exec request the instance or endpoint refused.
// ExitCode is set when the command could not be started (127, 126).
type ExecRejectedError struct {
	Message  string
	Code     string // e.g. ReplyCodeNotReady
	ExitCode int
}

func (e *ExecRejectedError) Error() string { return "exec via the dworm instance: " + e.Message }

// ExecSession is a process started over the instance socket in framed mode.
type ExecSession struct {
	ID     string
	conn   net.Conn
	reader *bufio.Reader
	mu     sync.Mutex
}

// StartExec starts req in framed mode.
func StartExec(socketPath string, req protocol.ExecRequest) (*ExecSession, error) {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrExecSocketUnavailable, err)
	}
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	req.Version = protocol.ExecRequestVersion
	req.Mode = protocol.ExecModeFramed
	header, err := json.Marshal(req)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if _, err := conn.Write(append(header, '\n')); err != nil {
		conn.Close()
		return nil, fmt.Errorf("%w: %v", ErrExecSocketUnavailable, err)
	}
	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("%w: %v", ErrExecSocketUnavailable, err)
	}
	var reply protocol.ExecReply
	if err := json.Unmarshal(line, &reply); err != nil {
		conn.Close()
		return nil, fmt.Errorf("%w: invalid reply: %v", ErrExecSocketUnavailable, err)
	}
	if !reply.OK {
		conn.Close()
		return nil, &ExecRejectedError{Message: reply.Error, Code: reply.Code, ExitCode: reply.ExitCode}
	}
	conn.SetDeadline(time.Time{})
	return &ExecSession{ID: reply.ID, conn: conn, reader: reader}, nil
}

func (s *ExecSession) writeFrame(frameType byte, payload []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return protocol.WriteFrame(s.conn, frameType, payload)
}

// Write sends stdin bytes.
func (s *ExecSession) Write(p []byte) (int, error) {
	for sent := 0; sent < len(p); {
		n := min(len(p)-sent, 32*1024)
		if err := s.writeFrame(protocol.FrameStdin, p[sent:sent+n]); err != nil {
			return sent, err
		}
		sent += n
	}
	return len(p), nil
}

// CloseStdin sends stdin EOF.
func (s *ExecSession) CloseStdin() error { return s.writeFrame(protocol.FrameStdinEOF, nil) }

// Signal sends a signal (name without SIG) to the process group.
func (s *ExecSession) Signal(name string) error {
	payload, _ := json.Marshal(protocol.ExecSignal{Signal: name})
	return s.writeFrame(protocol.FrameSignal, payload)
}

// Resize changes the size of the session's PTY.
func (s *ExecSession) Resize(rows, cols int) error {
	payload, _ := json.Marshal(protocol.ExecResize{Rows: rows, Cols: cols})
	return s.writeFrame(protocol.FrameResize, payload)
}

// ReadFrame returns the next output frame (stdout, stderr, or exit).
func (s *ExecSession) ReadFrame() (byte, []byte, error) { return protocol.ReadFrame(s.reader) }

// Close disconnects; a process that has not exited yet is terminated.
func (s *ExecSession) Close() error { return s.conn.Close() }

// ParseExit decodes an exit frame payload.
func ParseExit(payload []byte) *protocol.ExecExit { return parseExit(payload) }

// reportStartError turns a rejected start into the command's exit status.
func reportStartError(err error, stderr io.Writer) error {
	var rejected *ExecRejectedError
	if errors.As(err, &rejected) && rejected.ExitCode != 0 {
		fmt.Fprintf(stderr, "dworm: %s\n", rejected.Message)
		return &ExitError{Code: rejected.ExitCode}
	}
	return err
}

// ExitLost is the exit status of a command that was started but whose end
// dworm could not observe: the bridge to the container or the connection to
// the instance was lost.
const ExitLost = 255

// lostResult reports a connection to the instance that ended before the exit
// frame. The command was started, so it is not an error to retry.
func lostResult(err error, stderr io.Writer) error {
	fmt.Fprintf(stderr, "dworm: lost connection to the dworm instance before the command exited: %v\n", err)
	return &ExitError{Code: ExitLost}
}

// exitResult reports an exit frame as the command's exit status.
func exitResult(exit *protocol.ExecExit, stderr io.Writer) error {
	if exit.Error != "" {
		fmt.Fprintf(stderr, "dworm: %s\n", exit.Error)
	}
	if exit.Code != 0 {
		return &ExitError{Code: exit.Code}
	}
	return nil
}

// ExecViaSocket runs a command through the instance (framed mode),
// forwarding stdio and SIGINT/SIGTERM/SIGHUP. A non-zero exit status is
// returned as *ExitError.
func ExecViaSocket(socketPath string, req protocol.ExecRequest, stdin io.Reader, stdout, stderr io.Writer) error {
	session, err := StartExec(socketPath, req)
	if err != nil {
		return reportStartError(err, stderr)
	}
	defer session.Close()

	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := stdin.Read(buf)
			if n > 0 && session.writeFrame(protocol.FrameStdin, buf[:n]) != nil {
				return
			}
			if err != nil {
				session.CloseStdin()
				return
			}
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigCh)
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case sig := <-sigCh:
				session.Signal(strings.TrimPrefix(unixSignalName(sig), "SIG"))
			case <-done:
				return
			}
		}
	}()

	for {
		frameType, payload, err := session.ReadFrame()
		if err != nil {
			return lostResult(err, stderr)
		}
		switch frameType {
		case protocol.FrameStdout:
			stdout.Write(payload)
		case protocol.FrameStderr:
			stderr.Write(payload)
		case protocol.FrameExit:
			return exitResult(parseExit(payload), stderr)
		}
	}
}

// ExecTTY runs a command on a PTY in the container, attached to this
// process's stdio. If stdin is a terminal, the terminal is
// in raw mode meanwhile, so keys like Ctrl-C reach the command; window size
// changes are forwarded. SIGTERM and SIGINT are forwarded as signals, SIGHUP
// hangs up the session.
func ExecTTY(socketPath string, req protocol.ExecRequest) error {
	// Explicit -t also works with redirected stdio. Only a real local terminal
	// gets raw mode or resize handling; otherwise use a conventional initial size.
	fd := -1
	for _, file := range []*os.File{os.Stdout, os.Stdin} {
		if term.IsTerminal(int(file.Fd())) {
			fd = int(file.Fd())
			break
		}
	}
	cols, rows := 80, 24
	if fd >= 0 {
		var err error
		cols, rows, err = term.GetSize(fd)
		if err != nil {
			return fmt.Errorf("get terminal size: %w", err)
		}
	}
	termName := os.Getenv("TERM")
	if termName == "" {
		termName = "xterm-256color"
	}
	req.TTY = &protocol.ExecTTY{Rows: rows, Cols: cols, Term: termName}
	// Install handlers before raw mode, including while starting the command.
	sigCh := make(chan os.Signal, 8)
	signal.Notify(sigCh, syscall.SIGWINCH, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGTSTP)
	defer signal.Stop(sigCh)
	// Raw mode before the start: a failure here must not leave a started
	// command behind.
	var oldState *term.State
	if term.IsTerminal(int(os.Stdin.Fd())) {
		var err error
		oldState, err = term.MakeRaw(int(os.Stdin.Fd()))
		if err != nil {
			return fmt.Errorf("set raw mode: %w", err)
		}
	}
	restored := false
	restore := func() {
		if !restored && oldState != nil {
			term.Restore(int(os.Stdin.Fd()), oldState)
			restored = true
		}
	}
	defer restore()
	session, err := StartExec(socketPath, req)
	if err != nil {
		restore()
		return reportStartError(err, os.Stderr)
	}
	defer session.Close()

	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 && session.writeFrame(protocol.FrameStdin, buf[:n]) != nil {
				return
			}
			if err != nil {
				return
			}
		}
	}()

	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case sig := <-sigCh:
				switch sig {
				case syscall.SIGWINCH:
					if cols, rows, err := term.GetSize(fd); fd >= 0 && err == nil {
						session.Resize(rows, cols)
					}
				case syscall.SIGHUP:
					session.Close()
				default:
					session.Signal(strings.TrimPrefix(unixSignalName(sig), "SIG"))
				}
			case <-done:
				return
			}
		}
	}()

	for {
		frameType, payload, err := session.ReadFrame()
		if err != nil {
			restore()
			return lostResult(err, os.Stderr)
		}
		switch frameType {
		case protocol.FrameStdout, protocol.FrameStderr:
			os.Stdout.Write(payload)
		case protocol.FrameExit:
			restore()
			return exitResult(parseExit(payload), os.Stderr)
		}
	}
}

func unixSignalName(sig os.Signal) string {
	switch sig {
	case syscall.SIGINT:
		return "INT"
	case syscall.SIGTERM:
		return "TERM"
	case syscall.SIGHUP:
		return "HUP"
	case syscall.SIGQUIT:
		return "QUIT"
	case syscall.SIGTSTP:
		return "TSTP"
	default:
		return sig.String()
	}
}
