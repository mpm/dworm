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

	"github.com/mpm/dworm/internal/protocol"
)

// ErrExecSocketUnavailable means the exec socket could not be used and the
// command was not started; callers may fall back to `docker exec`.
var ErrExecSocketUnavailable = errors.New("dworm up exec socket unavailable")

// ExecViaSocket runs a command through a running `dworm up` (framed mode),
// forwarding stdio and SIGINT/SIGTERM/SIGHUP. A non-zero exit status is
// returned as *ExitError.
func ExecViaSocket(socketPath string, req protocol.ExecRequest, stdin io.Reader, stdout, stderr io.Writer) error {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrExecSocketUnavailable, err)
	}
	defer conn.Close()

	req.Version = protocol.ExecRequestVersion
	req.Mode = protocol.ExecModeFramed
	header, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if _, err := conn.Write(append(header, '\n')); err != nil {
		return fmt.Errorf("%w: %v", ErrExecSocketUnavailable, err)
	}
	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("%w: %v", ErrExecSocketUnavailable, err)
	}
	var reply protocol.ExecReply
	if err := json.Unmarshal(line, &reply); err != nil {
		return fmt.Errorf("%w: invalid reply: %v", ErrExecSocketUnavailable, err)
	}
	if !reply.OK {
		if reply.ExitCode != 0 {
			fmt.Fprintf(stderr, "dworm: %s\n", reply.Error)
			return &ExitError{Code: reply.ExitCode}
		}
		return fmt.Errorf("exec via dworm up: %s", reply.Error)
	}

	var writeMu sync.Mutex
	writeFrame := func(frameType byte, payload []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return protocol.WriteFrame(conn, frameType, payload)
	}

	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := stdin.Read(buf)
			if n > 0 && writeFrame(protocol.FrameStdin, buf[:n]) != nil {
				return
			}
			if err != nil {
				writeFrame(protocol.FrameStdinEOF, nil)
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
				name := strings.TrimPrefix(unixSignalName(sig), "SIG")
				payload, _ := json.Marshal(protocol.ExecSignal{Signal: name})
				writeFrame(protocol.FrameSignal, payload)
			case <-done:
				return
			}
		}
	}()

	for {
		frameType, payload, err := protocol.ReadFrame(reader)
		if err != nil {
			return fmt.Errorf("lost connection to dworm up before the command exited: %w", err)
		}
		switch frameType {
		case protocol.FrameStdout:
			stdout.Write(payload)
		case protocol.FrameStderr:
			stderr.Write(payload)
		case protocol.FrameExit:
			exit := parseExit(payload)
			if exit.Code != 0 {
				return &ExitError{Code: exit.Code}
			}
			return nil
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
	default:
		return sig.String()
	}
}
