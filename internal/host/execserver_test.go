//go:build linux

package host

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mpm/dworm/internal/protocol"
	"github.com/mpm/dworm/internal/protocol/testutil"
)

// fakeExecEndpoint answers exec streams like `cat` that writes "err\n" to
// stderr first and exits 3 on stdin EOF. With ignoreEOF it never exits, like a
// process that ignores stdin EOF.
type fakeExecEndpoint struct {
	ignoreEOF bool
	reject    string

	mu       sync.Mutex
	requests []protocol.ExecRequest
	killed   chan string // receives the exec ID when the stream closes before exit
}

func (f *fakeExecEndpoint) serve(mux *protocol.Mux) {
	for {
		stream, err := mux.AcceptStream()
		if err != nil {
			return
		}
		go f.handle(stream)
	}
}

func (f *fakeExecEndpoint) handle(stream net.Conn) {
	defer stream.Close()
	marker := make([]byte, 1)
	if _, err := io.ReadFull(stream, marker); err != nil || marker[0] != protocol.StreamTypeExec {
		return
	}
	var req protocol.ExecRequest
	if err := protocol.ReadJSONMessage(stream, protocol.MaxExecHeaderSize, &req); err != nil {
		return
	}
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	if f.reject != "" {
		protocol.WriteJSONMessage(stream, protocol.ExecReply{Error: f.reject})
		return
	}
	protocol.WriteJSONMessage(stream, protocol.ExecReply{OK: true, ID: req.ID})
	protocol.WriteFrame(stream, protocol.FrameStderr, []byte("err\n"))
	for {
		frameType, payload, err := protocol.ReadFrame(stream)
		if err != nil {
			f.killed <- req.ID
			return
		}
		switch frameType {
		case protocol.FrameStdin:
			protocol.WriteFrame(stream, protocol.FrameStdout, payload)
		case protocol.FrameSignal, protocol.FrameResize:
			protocol.WriteFrame(stream, protocol.FrameStdout, payload)
		case protocol.FrameStdinEOF:
			if !f.ignoresEOF() {
				protocol.WriteJSONFrame(stream, protocol.FrameExit, protocol.ExecExit{Code: 3})
				return
			}
		}
	}
}

func (f *fakeExecEndpoint) ignoresEOF() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ignoreEOF
}

// setIgnoreEOF changes ignoreEOF while the endpoint serves.
func (f *fakeExecEndpoint) setIgnoreEOF(ignore bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ignoreEOF = ignore
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type execServerFixture struct {
	h        *testutil.TestHarness
	server   *ExecServer
	endpoint *fakeExecEndpoint
	logs     *syncBuffer
}

func newExecServerFixture(t *testing.T, endpoint *fakeExecEndpoint) *execServerFixture {
	t.Helper()
	h, err := testutil.NewTestHarness()
	if err != nil {
		t.Fatal(err)
	}
	endpoint.killed = make(chan string, 10)
	go endpoint.serve(h.EndpointMux)
	logs := &syncBuffer{}
	path := filepath.Join(t.TempDir(), "exec.sock")
	server, err := NewExecServer(path, ExecServerConfig{
		Open:            h.HostMux.OpenStream,
		WorkspaceFolder: func() string { return "/workspaces/app" },
		Logger:          log.New(logs, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		server.Close()
		h.Close()
	})
	return &execServerFixture{h: h, server: server, endpoint: endpoint, logs: logs}
}

func (f *execServerFixture) dial(t *testing.T, header string) (*net.UnixConn, *bufio.Reader, protocol.ExecReply) {
	t.Helper()
	conn, err := net.Dial("unix", f.server.Path())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write([]byte(header + "\n")); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	var reply protocol.ExecReply
	if err := json.Unmarshal(line, &reply); err != nil {
		t.Fatalf("reply %q: %v", line, err)
	}
	return conn.(*net.UnixConn), reader, reply
}

func expectKilled(t *testing.T, f *fakeExecEndpoint, within time.Duration) {
	t.Helper()
	select {
	case <-f.killed:
	case <-time.After(within):
		t.Fatal("bridge stream was not closed after the client disconnected")
	}
}

func expectNotKilled(t *testing.T, f *fakeExecEndpoint, during time.Duration) {
	t.Helper()
	select {
	case <-f.killed:
		t.Fatal("bridge stream closed while the client was still connected")
	case <-time.After(during):
	}
}

func TestExecSocketMode(t *testing.T) {
	f := newExecServerFixture(t, &fakeExecEndpoint{})
	info, err := os.Stat(f.server.Path())
	if err != nil || info.Mode().Perm() != 0600 || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("socket mode = %v, %v", info.Mode(), err)
	}
	f.server.Close()
	if _, err := os.Stat(f.server.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket remains after Close: %v", err)
	}
}

func TestExecRawModeTranslatesStreams(t *testing.T) {
	f := newExecServerFixture(t, &fakeExecEndpoint{})
	conn, reader, reply := f.dial(t, `{"version":1,"argv":["opencode","acp"],"env":{"FOO":"bar"},"mode":"raw"}`)
	if !reply.OK || reply.ID == "" {
		t.Fatalf("reply = %+v", reply)
	}
	conn.Write([]byte("a\n"))
	conn.Write([]byte("b\n"))
	conn.CloseWrite()
	out, err := io.ReadAll(reader)
	if err != nil || string(out) != "a\nb\n" {
		t.Fatalf("stdout = %q, %v; want exactly the input", out, err)
	}

	req := f.endpoint.requests[0]
	if req.Cwd != "/workspaces/app" || req.Env["FOO"] != "bar" || req.ID != reply.ID || req.Mode != "" {
		t.Fatalf("bridge request = %+v", req)
	}
	deadline := time.Now().Add(time.Second)
	for !strings.Contains(f.logs.String(), "Exited with code 3") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	logs := f.logs.String()
	if !strings.Contains(logs, "[exec "+reply.ID+"] err\n") || !strings.Contains(logs, "Exited with code 3") {
		t.Fatalf("logs = %q", logs)
	}
}

func TestExecRawHalfCloseIsStdinEOFButDisconnectKills(t *testing.T) {
	f := newExecServerFixture(t, &fakeExecEndpoint{ignoreEOF: true})
	conn, _, reply := f.dial(t, `{"version":1,"argv":["sleep"]}`)
	if !reply.OK {
		t.Fatalf("reply = %+v", reply)
	}
	conn.CloseWrite()
	expectNotKilled(t, f.endpoint, 700*time.Millisecond)
	conn.Close()
	expectKilled(t, f.endpoint, 2*time.Second)
}

func TestExecRawDisconnectWithoutHalfCloseKills(t *testing.T) {
	f := newExecServerFixture(t, &fakeExecEndpoint{ignoreEOF: true})
	conn, _, _ := f.dial(t, `{"version":1,"argv":["sleep"]}`)
	conn.Close()
	expectKilled(t, f.endpoint, 2*time.Second)
}

func TestExecFramedMode(t *testing.T) {
	f := newExecServerFixture(t, &fakeExecEndpoint{})
	conn, reader, reply := f.dial(t, `{"version":1,"argv":["cat"],"cwd":"/tmp","mode":"framed"}`)
	if !reply.OK {
		t.Fatalf("reply = %+v", reply)
	}
	protocol.WriteFrame(conn, protocol.FrameStdin, []byte("x"))
	protocol.WriteJSONFrame(conn, protocol.FrameSignal, protocol.ExecSignal{Signal: "TERM"})
	protocol.WriteFrame(conn, protocol.FrameStdinEOF, nil)
	var got []string
	for {
		frameType, payload, err := protocol.ReadFrame(reader)
		if err != nil {
			t.Fatalf("read frame: %v (got %q)", err, got)
		}
		got = append(got, fmt.Sprintf("%d:%s", frameType, payload))
		if frameType == protocol.FrameExit {
			break
		}
	}
	want := []string{`2:err` + "\n", `1:x`, `1:{"signal":"TERM"}`, `4:{"code":3}`}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("frames = %q, want %q", got, want)
	}
	if f.endpoint.requests[0].Cwd != "/tmp" {
		t.Fatalf("cwd = %q", f.endpoint.requests[0].Cwd)
	}
}

func TestExecFramedDisconnectKills(t *testing.T) {
	f := newExecServerFixture(t, &fakeExecEndpoint{ignoreEOF: true})
	conn, _, _ := f.dial(t, `{"version":1,"argv":["sleep"],"mode":"framed"}`)
	protocol.WriteFrame(conn, protocol.FrameStdinEOF, nil)
	expectNotKilled(t, f.endpoint, 300*time.Millisecond)
	conn.Close()
	expectKilled(t, f.endpoint, 2*time.Second)
}

func TestExecTTYRequest(t *testing.T) {
	f := newExecServerFixture(t, &fakeExecEndpoint{})
	_, _, reply := f.dial(t, `{"version":1,"argv":["bash"],"tty":{"rows":24,"cols":80}}`)
	if reply.OK || !strings.Contains(reply.Error, "tty requires framed mode") {
		t.Fatalf("raw tty reply = %+v", reply)
	}
	conn, reader, reply := f.dial(t, `{"version":1,"argv":["bash"],"mode":"framed","tty":{"rows":24,"cols":80,"term":"xterm"}}`)
	if !reply.OK {
		t.Fatalf("framed tty reply = %+v", reply)
	}
	if tty := f.endpoint.requests[0].TTY; tty == nil || tty.Rows != 24 || tty.Cols != 80 || tty.Term != "xterm" {
		t.Fatalf("bridge request tty = %+v", tty)
	}
	protocol.ReadFrame(reader) // stderr "err"
	protocol.WriteJSONFrame(conn, protocol.FrameResize, protocol.ExecResize{Rows: 40, Cols: 100})
	if frameType, payload, err := protocol.ReadFrame(reader); err != nil || frameType != protocol.FrameStdout || string(payload) != `{"rows":40,"cols":100}` {
		t.Fatalf("resize did not reach the endpoint: %d %s %v", frameType, payload, err)
	}
}

func TestExecFramedBridgeLost(t *testing.T) {
	f := newExecServerFixture(t, &fakeExecEndpoint{ignoreEOF: true})
	conn, reader, reply := f.dial(t, `{"version":1,"argv":["sleep"],"mode":"framed"}`)
	if !reply.OK {
		t.Fatalf("reply = %+v", reply)
	}
	protocol.ReadFrame(reader) // stderr "err"
	f.h.EndpointMux.Close()
	frameType, payload, err := protocol.ReadFrame(reader)
	if err != nil || frameType != protocol.FrameExit || string(payload) != `{"code":255,"error":"bridge lost"}` {
		t.Fatalf("frame = %d %s, %v; want the bridge-lost exit frame", frameType, payload, err)
	}
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := protocol.ReadFrame(reader); err != io.EOF {
		t.Fatalf("after exit frame: %v, want EOF", err)
	}
}

func TestExecRawBridgeLostClosesConnection(t *testing.T) {
	f := newExecServerFixture(t, &fakeExecEndpoint{ignoreEOF: true})
	conn, reader, _ := f.dial(t, `{"version":1,"argv":["sleep"]}`)
	f.h.EndpointMux.Close()
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadAll(reader); err != nil {
		t.Fatalf("raw connection after bridge loss: %v, want EOF", err)
	}
}

func TestExecRejectsInvalidRequests(t *testing.T) {
	f := newExecServerFixture(t, &fakeExecEndpoint{})
	for header, want := range map[string]string{
		`{"version":2,"argv":["true"]}`:                   "unsupported request version",
		`{"version":1,"argv":[]}`:                         "argv must not be empty",
		`{"version":1,"argv":["true"],"mode":"pty"}`:      "unknown mode",
		`{"version":1,"argv":["true"],"env":{"A-B":"x"}}`: "invalid environment",
		`{"version":1,"argv":["true"],"id":"chosen"}`:     "id is assigned",
		`not json`: "invalid request",
		`{"version":1,"argv":["true"],"unexpected":true}`:                                  "invalid request",
		`{"version":1,"argv":["` + strings.Repeat("x", protocol.MaxExecHeaderSize) + `"]}`: "exceeds",
	} {
		_, _, reply := f.dial(t, header)
		if reply.OK || !strings.Contains(reply.Error, want) {
			t.Errorf("header %.40q: reply = %+v, want error containing %q", header, reply, want)
		}
	}
	if len(f.endpoint.requests) != 0 {
		t.Fatalf("invalid requests reached the endpoint: %+v", f.endpoint.requests)
	}
}

func TestExecPassesEndpointRejection(t *testing.T) {
	f := newExecServerFixture(t, &fakeExecEndpoint{reject: "too many concurrent execs (limit 32)"})
	_, _, reply := f.dial(t, `{"version":1,"argv":["true"]}`)
	if reply.OK || reply.Error != "too many concurrent execs (limit 32)" {
		t.Fatalf("reply = %+v", reply)
	}
}

func TestExecConcurrentRawSessions(t *testing.T) {
	f := newExecServerFixture(t, &fakeExecEndpoint{})
	var wg sync.WaitGroup
	for i := range 10 {
		conn, reader, reply := f.dial(t, `{"version":1,"argv":["cat"]}`)
		if !reply.OK {
			t.Fatalf("reply = %+v", reply)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			want := fmt.Sprintf("session %d", i)
			conn.Write([]byte(want))
			conn.CloseWrite()
			if out, _ := io.ReadAll(reader); string(out) != want {
				t.Errorf("session %d stdout = %q", i, out)
			}
		}()
	}
	wg.Wait()
}

func TestExecServerCloseEndsSessions(t *testing.T) {
	f := newExecServerFixture(t, &fakeExecEndpoint{ignoreEOF: true})
	f.dial(t, `{"version":1,"argv":["sleep"]}`)
	f.server.Close()
	expectKilled(t, f.endpoint, 2*time.Second)
}

func TestExecViaSocket(t *testing.T) {
	f := newExecServerFixture(t, &fakeExecEndpoint{})
	var stdout, stderr bytes.Buffer
	err := ExecViaSocket(f.server.Path(), protocol.ExecRequest{Argv: []string{"cat"}}, strings.NewReader("hello"), &stdout, &stderr)
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 3 {
		t.Fatalf("err = %v, want exit code 3", err)
	}
	if stdout.String() != "hello" || stderr.String() != "err\n" {
		t.Fatalf("stdout = %q, stderr = %q", stdout.String(), stderr.String())
	}

	err = ExecViaSocket(filepath.Join(t.TempDir(), "missing.sock"), protocol.ExecRequest{Argv: []string{"true"}}, strings.NewReader(""), &stdout, &stderr)
	if !errors.Is(err, ErrExecSocketUnavailable) {
		t.Fatalf("missing socket err = %v, want ErrExecSocketUnavailable", err)
	}
}

func TestExecLogWriterRateLimits(t *testing.T) {
	var logs bytes.Buffer
	now := time.Unix(0, 0)
	w := newExecLogWriter(log.New(&logs, "", 0), "[exec x] ")
	w.now = func() time.Time { return now }
	for i := range execLogLinesPerSecond + 5 {
		fmt.Fprintf(w, "line %d\n", i)
	}
	w.Write([]byte("partial"))
	now = now.Add(time.Second)
	w.Flush()
	got := logs.String()
	if strings.Count(got, "\n") != execLogLinesPerSecond+2 {
		t.Fatalf("logged %d lines:\n%s", strings.Count(got, "\n"), got)
	}
	if !strings.Contains(got, "[exec x] (5 stderr lines suppressed)") || !strings.Contains(got, "[exec x] partial\n") {
		t.Fatalf("logs = %s", got)
	}
}
