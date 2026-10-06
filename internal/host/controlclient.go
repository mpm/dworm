package host

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/mpm/dworm/internal/protocol"
)

// ErrLegacyInstance means the instance predates socket ops (dworm v0.7.x).
var ErrLegacyInstance = errors.New("the running instance is an older dworm without socket ops (v0.7.x)")

// ReplyError is a request rejected by the instance.
type ReplyError struct {
	Message string
	Code    string // e.g. ReplyCodeNotReady
}

func (e *ReplyError) Error() string { return e.Message }

// controlRequest sends one request line and reads the reply line.
func controlRequest(socketPath string, req ControlRequest, timeout time.Duration) (net.Conn, *bufio.Reader, *ControlReply, error) {
	conn, err := net.DialTimeout("unix", socketPath, 2*time.Second)
	if err != nil {
		return nil, nil, nil, err
	}
	req.Version = protocol.ExecRequestVersion
	header, err := json.Marshal(req)
	if err != nil {
		conn.Close()
		return nil, nil, nil, err
	}
	conn.SetDeadline(time.Now().Add(timeout))
	if _, err := conn.Write(append(header, '\n')); err != nil {
		conn.Close()
		return nil, nil, nil, err
	}
	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		conn.Close()
		return nil, nil, nil, fmt.Errorf("read reply: %w", err)
	}
	conn.SetDeadline(time.Time{})
	var reply ControlReply
	if err := json.Unmarshal(line, &reply); err != nil {
		conn.Close()
		return nil, nil, nil, fmt.Errorf("invalid reply: %w", err)
	}
	if !reply.OK {
		conn.Close()
		if strings.Contains(reply.Error, `unknown field "op"`) {
			return nil, nil, nil, ErrLegacyInstance
		}
		return nil, nil, nil, &ReplyError{Message: reply.Error, Code: reply.Code}
	}
	return conn, reader, &reply, nil
}

// QueryInstance returns the live state of the instance listening on
// socketPath (op status).
func QueryInstance(socketPath string) (*InstanceState, error) {
	conn, _, reply, err := controlRequest(socketPath, ControlRequest{Op: OpStatus}, 10*time.Second)
	if err != nil {
		return nil, err
	}
	conn.Close()
	if reply.Status == nil {
		return nil, errors.New("status reply without status")
	}
	return reply.Status, nil
}

// EventStream reads an instance's event stream (op events).
type EventStream struct {
	conn   net.Conn
	reader *bufio.Reader
}

// SubscribeEvents subscribes to the events of the instance listening on
// socketPath. The stream starts with a snapshot including up to history log
// events; without follow it ends after the snapshot.
func SubscribeEvents(socketPath string, history int, follow bool) (*EventStream, error) {
	conn, reader, _, err := controlRequest(socketPath, ControlRequest{Op: OpEvents, History: history, Follow: &follow}, 10*time.Second)
	if err != nil {
		return nil, err
	}
	return &EventStream{conn: conn, reader: reader}, nil
}

// NextRaw returns the next event as a JSON line (without the newline).
func (s *EventStream) NextRaw() ([]byte, error) {
	line, err := s.reader.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	return line[:len(line)-1], nil
}

// Next returns the next event.
func (s *EventStream) Next() (Event, error) {
	var e Event
	line, err := s.NextRaw()
	if err != nil {
		return e, err
	}
	return e, json.Unmarshal(line, &e)
}

// Close ends the subscription.
func (s *EventStream) Close() error { return s.conn.Close() }

// StopInstance stops the workspace's instance and waits until it has exited.
// It reports whether an instance was running. Instances that do not answer
// op stop (older versions) get SIGTERM.
func StopInstance(workspacePath string, timeout time.Duration) (bool, error) {
	paths := InstancePathsFor(workspacePath)
	running, err := InstanceRunning(paths)
	if err != nil || !running {
		return false, err
	}
	pid := 0
	if state, err := ReadInstanceState(paths.State); err == nil {
		pid = state.PID
	}
	conn, _, _, err := controlRequest(paths.Socket, ControlRequest{Op: OpStop}, 10*time.Second)
	if err == nil {
		// The instance closes the connection once it has shut down.
		conn.SetReadDeadline(time.Now().Add(timeout))
		conn.Read(make([]byte, 1))
		conn.Close()
	} else {
		if pid <= 0 {
			return true, fmt.Errorf("stop instance: %v", err)
		}
		process, findErr := os.FindProcess(pid)
		if findErr != nil {
			return true, findErr
		}
		if err := process.Signal(syscall.SIGTERM); err != nil {
			return true, fmt.Errorf("stop instance (pid %d): %w", pid, err)
		}
	}
	deadline := time.Now().Add(timeout)
	for {
		running, err := InstanceRunning(paths)
		if err != nil {
			return true, err
		}
		if !running {
			return true, nil
		}
		// Another instance may have started right after this one stopped.
		if state, err := ReadInstanceState(paths.State); err == nil && pid > 0 && state.PID != pid {
			return true, nil
		}
		if time.Now().After(deadline) {
			return true, fmt.Errorf("instance did not stop within %v", timeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
