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
	"time"

	"github.com/mpm/dworm/internal/config"
	"github.com/mpm/dworm/internal/protocol"
)

// ExecServer serves the local exec socket of a running `dworm up`. Each
// connection runs one process in the container over a bridge exec stream.
//
// A client sends one JSON line (protocol.ExecRequest) and receives one JSON
// line (protocol.ExecReply). In raw mode the connection then carries stdin
// (client half-close = stdin EOF) and stdout; stderr goes to the log. In
// framed mode it carries protocol frames. A client that disconnects before
// the process exits gets its process group terminated.
type ExecServer struct {
	path            string
	listener        *net.UnixListener
	open            func() (net.Conn, error)
	workspaceFolder string
	logger          *log.Logger
	headerTimeout   time.Duration

	mu       sync.Mutex
	closed   bool
	sessions map[io.Closer]struct{}
	wg       sync.WaitGroup
}

// NewExecServer listens on path (mode 0600) and serves until Close. open opens
// a new bridge stream; workspaceFolder is the default working directory.
func NewExecServer(path string, open func() (net.Conn, error), workspaceFolder string, logger *log.Logger) (*ExecServer, error) {
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
	s := &ExecServer{
		path:            path,
		listener:        listener,
		open:            open,
		workspaceFolder: workspaceFolder,
		logger:          logger,
		headerTimeout:   30 * time.Second,
		sessions:        make(map[io.Closer]struct{}),
	}
	go s.serve()
	return s, nil
}

// Path returns the socket path.
func (s *ExecServer) Path() string { return s.path }

// Close stops accepting connections, ends all sessions (terminating their
// processes), and removes the socket.
func (s *ExecServer) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	sessions := make([]io.Closer, 0, len(s.sessions))
	for c := range s.sessions {
		sessions = append(sessions, c)
	}
	s.mu.Unlock()

	s.listener.Close()
	os.Remove(s.path)
	for _, c := range sessions {
		c.Close()
	}
	s.wg.Wait()
}

func (s *ExecServer) track(c io.Closer) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.sessions[c] = struct{}{}
	return true
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
		if !s.track(conn) {
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

func writeReplyLine(w io.Writer, reply protocol.ExecReply) error {
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
func readRequest(reader *bufio.Reader) (*protocol.ExecRequest, error) {
	line, err := reader.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		return nil, fmt.Errorf("request header exceeds %d bytes", protocol.MaxExecHeaderSize)
	}
	if err != nil {
		return nil, fmt.Errorf("read request: %w", err)
	}
	var req protocol.ExecRequest
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}
	if req.Version != protocol.ExecRequestVersion {
		return nil, fmt.Errorf("unsupported request version %d (want %d)", req.Version, protocol.ExecRequestVersion)
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
	if req.Cwd == "" {
		req.Cwd = s.workspaceFolder
	}
	req.ID = newExecID()

	stream, err := s.open()
	if err != nil {
		s.reject(conn, "open bridge stream: %v", err)
		return
	}
	if !s.track(stream) {
		stream.Close()
		s.reject(conn, "dworm up is shutting down")
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

	var exit *protocol.ExecExit
	if mode == protocol.ExecModeRaw {
		exit = s.proxyRaw(conn, reader, stream, req.ID)
	} else {
		exit = s.proxyFramed(conn, reader, stream, req.ID)
	}
	switch {
	case exit == nil:
		s.logger.Printf("[exec %s] Ended without exit status (caller or bridge disconnected)", req.ID)
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
func (s *ExecServer) proxyFramed(conn *net.UnixConn, reader *bufio.Reader, stream net.Conn, id string) *protocol.ExecExit {
	exited := make(chan struct{})
	go func() {
		for {
			frameType, payload, err := protocol.ReadFrame(reader)
			if err != nil {
				select {
				case <-exited:
				default:
					stream.Close()
				}
				return
			}
			switch frameType {
			case protocol.FrameStdin, protocol.FrameStdinEOF, protocol.FrameSignal:
				if protocol.WriteFrame(stream, frameType, payload) != nil {
					return
				}
			default:
				s.logger.Printf("[exec %s] Client sent invalid frame type %d", id, frameType)
				stream.Close()
				conn.Close()
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
		case protocol.FrameStdout, protocol.FrameStderr, protocol.FrameExit:
			if frameType == protocol.FrameExit {
				close(exited)
			}
			if err := protocol.WriteFrame(conn, frameType, payload); err != nil {
				if frameType != protocol.FrameExit {
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
