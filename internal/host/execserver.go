package host

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mpm/dworm/internal/config"
	"github.com/mpm/dworm/internal/protocol"
)

// Socket ops. A request without "op" is an exec request (v0.7.0 clients).
const (
	OpExec   = "exec"
	OpEvents = "events"
	OpStatus = "status"
	OpStop   = "stop"
)

// ReplyCodeNotReady is the reply code for exec requests before the instance
// is ready.
const ReplyCodeNotReady = "not_ready"

// ControlRequest is the first line a client sends on the instance socket.
type ControlRequest struct {
	protocol.ExecRequest
	Op      string `json:"op,omitempty"`
	History int    `json:"history,omitempty"` // events: log events in the snapshot
	Follow  *bool  `json:"follow,omitempty"`  // events: false = snapshot only
}

// ControlReply is the first line the instance sends back.
type ControlReply struct {
	protocol.ExecReply
	Status *InstanceState `json:"status,omitempty"` // op status
}

// ExecServerConfig configures an ExecServer. Only Open is required.
type ExecServerConfig struct {
	// Open opens a new bridge stream.
	Open func() (net.Conn, error)
	// WorkspaceFolder returns the default working directory.
	WorkspaceFolder func() string
	Logger          *log.Logger
	// Ready reports whether exec requests can be served; before that they
	// are rejected with ReplyCodeNotReady.
	Ready func() bool
	// Status returns the live instance state (op status).
	Status func() InstanceState
	// Events backs op events.
	Events *EventBus
	// Stop asks the instance to shut down (op stop).
	Stop func()
}

type sessionKind int

const (
	sessionExec    sessionKind = iota // exec client connection or bridge stream
	sessionEvents                     // event subscriber
	sessionControl                    // other ops
)

// ExecServer serves the local socket of a running instance. Each connection
// carries one op; an exec connection runs one process in the container over a
// bridge exec stream.
//
// A client sends one JSON line (ControlRequest) and receives one JSON line
// (ControlReply). In raw mode the connection then carries stdin (client
// half-close = stdin EOF) and stdout; stderr goes to the log. In framed mode
// it carries protocol frames. A client that disconnects before the process
// exits gets its process group terminated.
type ExecServer struct {
	path          string
	listener      *net.UnixListener
	cfg           ExecServerConfig
	logger        *log.Logger
	headerTimeout time.Duration
	done          chan struct{}

	mu          sync.Mutex
	closed      bool
	execsClosed bool
	clients     int
	sessions    map[io.Closer]sessionKind
	wg          sync.WaitGroup
}

// NewExecServer listens on path (mode 0600) and serves until Close.
func NewExecServer(path string, cfg ExecServerConfig) (*ExecServer, error) {
	// A crashed instance may have left its socket behind; the caller holds the
	// workspace lock, so nothing else is using it.
	os.Remove(path)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("listen on exec socket: %w", err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		listener.Close()
		return nil, fmt.Errorf("secure exec socket: %w", err)
	}
	logger := cfg.Logger
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	s := &ExecServer{
		path:          path,
		listener:      listener,
		cfg:           cfg,
		logger:        logger,
		headerTimeout: 30 * time.Second,
		done:          make(chan struct{}),
		sessions:      make(map[io.Closer]sessionKind),
	}
	go s.serve()
	return s, nil
}

// Path returns the socket path.
func (s *ExecServer) Path() string { return s.path }

// Clients returns the number of attached clients: running exec sessions and
// event subscriptions.
func (s *ExecServer) Clients() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clients
}

func (s *ExecServer) attach(delta int) {
	s.mu.Lock()
	s.clients += delta
	attached := s.clients
	s.mu.Unlock()
	s.publish(Event{Type: EventClient, Attached: attached})
}

func (s *ExecServer) publish(e Event) {
	if s.cfg.Events != nil {
		s.cfg.Events.Publish(e)
	}
}

// CloseExecs rejects new exec requests and ends all exec sessions, which
// terminates their processes. Other ops keep working until Close.
func (s *ExecServer) CloseExecs() {
	s.mu.Lock()
	s.execsClosed = true
	var sessions []io.Closer
	for c, kind := range s.sessions {
		if kind == sessionExec {
			sessions = append(sessions, c)
		}
	}
	s.mu.Unlock()
	for _, c := range sessions {
		c.Close()
	}
}

// Close stops accepting connections, ends all sessions (terminating their
// processes), and removes the socket. Event subscribers get up to two
// seconds to receive their remaining events; close the event bus first.
func (s *ExecServer) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()

	s.listener.Close()
	os.Remove(s.path)
	s.CloseExecs()
	close(s.done)

	drained := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(drained)
	}()
	select {
	case <-drained:
		return
	case <-time.After(2 * time.Second):
	}
	s.mu.Lock()
	sessions := make([]io.Closer, 0, len(s.sessions))
	for c := range s.sessions {
		sessions = append(sessions, c)
	}
	s.mu.Unlock()
	for _, c := range sessions {
		c.Close()
	}
	<-drained
}

func (s *ExecServer) track(c io.Closer, kind sessionKind) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || (kind == sessionExec && s.execsClosed) {
		return false
	}
	s.sessions[c] = kind
	return true
}

func (s *ExecServer) retrack(c io.Closer, kind sessionKind) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[c]; ok {
		s.sessions[c] = kind
	}
}

func (s *ExecServer) untrack(c io.Closer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, c)
}

func (s *ExecServer) serve() {
	for {
		conn, err := s.listener.AcceptUnix()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return
			}
			s.logger.Printf("[exec] Accept failed: %v", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if !s.track(conn, sessionControl) {
			conn.Close()
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer s.untrack(conn)
			defer conn.Close()
			s.handle(conn)
		}()
	}
}

func writeReplyLine(w io.Writer, reply interface{}) error {
	data, err := json.Marshal(reply)
	if err != nil {
		return err
	}
	_, err = w.Write(append(data, '\n'))
	return err
}

func (s *ExecServer) reject(conn net.Conn, format string, args ...interface{}) {
	writeReplyLine(conn, protocol.ExecReply{Error: fmt.Sprintf(format, args...)})
}

// readRequest reads and validates the client's request line.
func readRequest(reader *bufio.Reader) (*ControlRequest, error) {
	line, err := reader.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		return nil, fmt.Errorf("request header exceeds %d bytes", protocol.MaxExecHeaderSize)
	}
	if err != nil {
		return nil, fmt.Errorf("read request: %w", err)
	}
	var req ControlRequest
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}
	if req.Version != protocol.ExecRequestVersion {
		return nil, fmt.Errorf("unsupported request version %d (want %d)", req.Version, protocol.ExecRequestVersion)
	}
	switch req.Op {
	case "":
		req.Op = OpExec
	case OpExec, OpEvents, OpStatus, OpStop:
	default:
		return nil, fmt.Errorf("unknown op %q", req.Op)
	}
	if req.Op != OpExec {
		return &req, nil
	}
	if len(req.Argv) == 0 {
		return nil, errors.New("argv must not be empty")
	}
	if req.Mode == "" {
		req.Mode = protocol.ExecModeRaw
	}
	if req.Mode != protocol.ExecModeRaw && req.Mode != protocol.ExecModeFramed {
		return nil, fmt.Errorf("unknown mode %q", req.Mode)
	}
	if req.ID != "" {
		return nil, errors.New("id is assigned by dworm")
	}
	if err := config.Validate(req.Env); err != nil {
		return nil, err
	}
	return &req, nil
}

func newExecID() string {
	buf := make([]byte, 6)
	rand.Read(buf)
	return hex.EncodeToString(buf)
}

func (s *ExecServer) handle(conn *net.UnixConn) {
	if err := checkPeer(conn); err != nil {
		s.logger.Printf("[exec] Rejected connection: %v", err)
		return
	}
	conn.SetReadDeadline(time.Now().Add(s.headerTimeout))
	reader := bufio.NewReaderSize(conn, protocol.MaxExecHeaderSize)
	req, err := readRequest(reader)
	if err != nil {
		s.reject(conn, "%v", err)
		return
	}
	conn.SetReadDeadline(time.Time{})
	switch req.Op {
	case OpEvents:
		s.handleEvents(conn, req)
	case OpStatus:
		if s.cfg.Status == nil {
			s.reject(conn, "op status is not supported")
			return
		}
		status := s.cfg.Status()
		status.Clients = s.Clients()
		writeReplyLine(conn, ControlReply{ExecReply: protocol.ExecReply{OK: true}, Status: &status})
	case OpStop:
		if s.cfg.Stop == nil {
			s.reject(conn, "op stop is not supported")
			return
		}
		writeReplyLine(conn, protocol.ExecReply{OK: true})
		s.cfg.Stop()
		// The connection closes when the instance has shut down.
		<-s.done
	default:
		s.retrack(conn, sessionExec)
		s.handleExec(conn, reader, &req.ExecRequest)
	}
}

// handleEvents streams events as JSON lines: first the snapshot, then live
// events unless follow is false. Closing the connection unsubscribes.
func (s *ExecServer) handleEvents(conn *net.UnixConn, req *ControlRequest) {
	if s.cfg.Events == nil {
		s.reject(conn, "op events is not supported")
		return
	}
	if req.History < 0 || req.History > EventHistorySize {
		s.reject(conn, "history must be between 0 and %d", EventHistorySize)
		return
	}
	sub := s.cfg.Events.Subscribe(req.History)
	defer sub.Close()
	s.retrack(conn, sessionEvents)
	if writeReplyLine(conn, protocol.ExecReply{OK: true}) != nil {
		return
	}
	writer := bufio.NewWriter(conn)
	writeEvent := func(e Event) {
		if data, err := json.Marshal(e); err == nil {
			writer.Write(append(data, '\n'))
		}
	}
	if req.Follow != nil && !*req.Follow {
		for i := 0; i < sub.Snapshot; i++ {
			writeEvent(<-sub.Events())
		}
		writer.Flush()
		return
	}
	s.attach(1)
	defer s.attach(-1)
	go func() {
		// Any input or EOF from the client ends the subscription.
		conn.Read(make([]byte, 1))
		sub.Close()
	}()
	for e := range sub.Events() {
		writeEvent(e)
		if len(sub.Events()) == 0 && writer.Flush() != nil {
			return
		}
	}
	writer.Flush()
}

func (s *ExecServer) handleExec(conn *net.UnixConn, reader *bufio.Reader, req *protocol.ExecRequest) {
	if s.cfg.Ready != nil && !s.cfg.Ready() {
		writeReplyLine(conn, protocol.ExecReply{Error: "not ready", Code: ReplyCodeNotReady})
		return
	}
	if req.Cwd == "" && s.cfg.WorkspaceFolder != nil {
		req.Cwd = s.cfg.WorkspaceFolder()
	}
	req.ID = newExecID()

	stream, err := s.cfg.Open()
	if err != nil {
		s.reject(conn, "open bridge stream: %v", err)
		return
	}
	if !s.track(stream, sessionExec) {
		stream.Close()
		s.reject(conn, "dworm instance is shutting down")
		return
	}
	defer s.untrack(stream)
	defer stream.Close()

	mode := req.Mode
	bridgeReq := *req
	bridgeReq.Mode = ""
	stream.SetDeadline(time.Now().Add(s.headerTimeout))
	if _, err := stream.Write([]byte{protocol.StreamTypeExec}); err != nil {
		s.reject(conn, "send request: %v", err)
		return
	}
	if err := protocol.WriteJSONMessage(stream, bridgeReq); err != nil {
		s.reject(conn, "send request: %v", err)
		return
	}
	var reply protocol.ExecReply
	if err := protocol.ReadJSONMessage(stream, protocol.MaxExecHeaderSize, &reply); err != nil {
		s.reject(conn, "read endpoint reply: %v", err)
		return
	}
	stream.SetDeadline(time.Time{})
	reply.ID = req.ID
	if err := writeReplyLine(conn, reply); err != nil || !reply.OK {
		if !reply.OK {
			s.logger.Printf("[exec %s] Rejected %q: %s", req.ID, req.Argv[0], reply.Error)
		}
		return
	}
	s.logger.Printf("[exec %s] Started %q (%s mode)", req.ID, req.Argv[0], mode)
	s.publish(Event{Type: EventExecStarted, ID: req.ID, Argv: req.Argv})
	s.attach(1)

	var exit *protocol.ExecExit
	if mode == protocol.ExecModeRaw {
		exit = s.proxyRaw(conn, reader, stream, req.ID)
	} else {
		exit = s.proxyFramed(conn, reader, stream, req.ID)
	}
	s.attach(-1)
	exited := Event{Type: EventExecExited, ID: req.ID, Code: -1, Error: "caller disconnected"}
	if exit != nil {
		exited.Code, exited.Signal, exited.Error = exit.Code, exit.Signal, exit.Error
	}
	s.publish(exited)
	switch {
	case exit == nil:
		s.logger.Printf("[exec %s] Ended without exit status (caller or bridge disconnected)", req.ID)
	case exit.Error != "":
		s.logger.Printf("[exec %s] Ended: %s", req.ID, exit.Error)
	case exit.Signal != "":
		s.logger.Printf("[exec %s] Exited on signal %s", req.ID, exit.Signal)
	default:
		s.logger.Printf("[exec %s] Exited with code %d", req.ID, exit.Code)
	}
}

func parseExit(payload []byte) *protocol.ExecExit {
	var exit protocol.ExecExit
	if err := json.Unmarshal(payload, &exit); err != nil {
		exit = protocol.ExecExit{Code: 255}
	}
	return &exit
}

// proxyRaw translates between a raw client and the framed bridge stream.
// Closing the bridge stream before the exit frame asks the endpoint to
// terminate the process group.
func (s *ExecServer) proxyRaw(conn *net.UnixConn, reader *bufio.Reader, stream net.Conn, id string) *protocol.ExecExit {
	stderrLog := newExecLogWriter(s.logger, fmt.Sprintf("[exec %s] ", id))
	defer stderrLog.Flush()
	exited := make(chan struct{})
	done := make(chan struct{})
	defer close(done)

	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := reader.Read(buf)
			if n > 0 && protocol.WriteFrame(stream, protocol.FrameStdin, buf[:n]) != nil {
				return
			}
			if err == io.EOF {
				// Half-close is stdin EOF; a full disconnect still kills.
				protocol.WriteFrame(stream, protocol.FrameStdinEOF, nil)
				select {
				case <-waitHangup(conn, done):
					stream.Close()
				case <-done:
				}
				return
			}
			if err != nil {
				select {
				case <-exited:
				default:
					stream.Close()
				}
				return
			}
		}
	}()

	for {
		frameType, payload, err := protocol.ReadFrame(stream)
		if err != nil {
			return nil
		}
		switch frameType {
		case protocol.FrameStdout:
			if _, err := conn.Write(payload); err != nil {
				stream.Close()
				return nil
			}
		case protocol.FrameStderr:
			stderrLog.Write(payload)
		case protocol.FrameExit:
			close(exited)
			return parseExit(payload)
		}
	}
}

// proxyFramed forwards frames between a framed client and the bridge stream.
// When the bridge fails, the client gets a final exit frame with code 255 and
// error "bridge lost".
func (s *ExecServer) proxyFramed(conn *net.UnixConn, reader *bufio.Reader, stream net.Conn, id string) *protocol.ExecExit {
	exited := make(chan struct{})
	var clientGone atomic.Bool
	go func() {
		for {
			frameType, payload, err := protocol.ReadFrame(reader)
			if err != nil {
				select {
				case <-exited:
				default:
					clientGone.Store(true)
					stream.Close()
				}
				return
			}
			switch frameType {
			case protocol.FrameStdin, protocol.FrameStdinEOF, protocol.FrameSignal, protocol.FrameResize:
				if protocol.WriteFrame(stream, frameType, payload) != nil {
					return
				}
			default:
				s.logger.Printf("[exec %s] Client sent invalid frame type %d", id, frameType)
				clientGone.Store(true)
				stream.Close()
				conn.Close()
				return
			}
		}
	}()

	for {
		frameType, payload, err := protocol.ReadFrame(stream)
		if err != nil {
			if clientGone.Load() {
				return nil
			}
			exit := &protocol.ExecExit{Code: 255, Error: protocol.ExecErrorBridgeLost}
			close(exited)
			protocol.WriteJSONFrame(conn, protocol.FrameExit, exit)
			return exit
		}
		switch frameType {
		case protocol.FrameStdout, protocol.FrameStderr, protocol.FrameExit:
			if frameType == protocol.FrameExit {
				close(exited)
			}
			if err := protocol.WriteFrame(conn, frameType, payload); err != nil {
				if frameType != protocol.FrameExit {
					clientGone.Store(true)
					stream.Close()
					return nil
				}
			}
			if frameType == protocol.FrameExit {
				return parseExit(payload)
			}
		}
	}
}

// execLogWriter logs process stderr line by line with a prefix, limited to
// execLogLinesPerSecond lines per second.
type execLogWriter struct {
	logger *log.Logger
	prefix string
	now    func() time.Time

	mu          sync.Mutex
	partial     []byte
	windowStart time.Time
	lines       int
	suppressed  int
}

const (
	execLogLinesPerSecond = 50
	execLogMaxLine        = 4096
)

func newExecLogWriter(logger *log.Logger, prefix string) *execLogWriter {
	return &execLogWriter{logger: logger, prefix: prefix, now: time.Now}
}

func (w *execLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.partial = append(w.partial, p...)
	for {
		i := bytes.IndexByte(w.partial, '\n')
		if i < 0 {
			break
		}
		w.emit(w.partial[:i])
		w.partial = w.partial[i+1:]
	}
	for len(w.partial) > execLogMaxLine {
		w.emit(w.partial[:execLogMaxLine])
		w.partial = w.partial[execLogMaxLine:]
	}
	return len(p), nil
}

// Flush logs a trailing partial line and any pending suppression notice.
func (w *execLogWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.partial) > 0 {
		w.emit(w.partial)
		w.partial = nil
	}
	w.reportSuppressed()
}

func (w *execLogWriter) emit(line []byte) {
	now := w.now()
	if now.Sub(w.windowStart) >= time.Second {
		w.reportSuppressed()
		w.windowStart = now
		w.lines = 0
	}
	if w.lines >= execLogLinesPerSecond {
		w.suppressed++
		return
	}
	w.lines++
	w.logger.Printf("%s%s", w.prefix, bytes.TrimRight(line, "\r"))
}

func (w *execLogWriter) reportSuppressed() {
	if w.suppressed > 0 {
		w.logger.Printf("%s(%d stderr lines suppressed)", w.prefix, w.suppressed)
		w.suppressed = 0
	}
}
