package endpoint

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mpm/dworm/internal/protocol"
	"github.com/mpm/dworm/internal/protocol/testutil"
)

type execFixture struct {
	t      *testing.T
	h      *testutil.TestHarness
	server *Server
}

func newExecFixture(t *testing.T) *execFixture {
	t.Helper()
	old := environmentDir
	environmentDir = filepath.Join(t.TempDir(), "dworm")
	t.Cleanup(func() { environmentDir = old })

	h, err := testutil.NewTestHarness()
	if err != nil {
		t.Fatalf("harness: %v", err)
	}
	server := NewServer()
	server.logger = log.New(io.Discard, "", 0)
	server.execs.logger = server.logger
	server.execs.grace = 300 * time.Millisecond
	go func() {
		for {
			stream, err := h.EndpointMux.AcceptStream()
			if err != nil {
				return
			}
			go server.handleStream(stream)
		}
	}()
	t.Cleanup(func() {
		server.execs.closeAll()
		h.Close()
	})
	return &execFixture{t: t, h: h, server: server}
}

// start opens an exec stream and returns it with the endpoint's reply.
func (f *execFixture) start(req protocol.ExecRequest) (net.Conn, protocol.ExecReply) {
	f.t.Helper()
	stream, err := f.h.HostMux.OpenStream()
	if err != nil {
		f.t.Fatalf("open stream: %v", err)
	}
	f.t.Cleanup(func() { stream.Close() })
	if _, err := stream.Write([]byte{protocol.StreamTypeExec}); err != nil {
		f.t.Fatal(err)
	}
	if err := protocol.WriteJSONMessage(stream, req); err != nil {
		f.t.Fatal(err)
	}
	var reply protocol.ExecReply
	if err := protocol.ReadJSONMessage(stream, protocol.MaxExecHeaderSize, &reply); err != nil {
		f.t.Fatalf("read reply: %v", err)
	}
	return stream, reply
}

func (f *execFixture) mustStart(argv ...string) net.Conn {
	f.t.Helper()
	stream, reply := f.start(protocol.ExecRequest{Version: 1, Argv: argv, ID: "test"})
	if !reply.OK {
		f.t.Fatalf("exec %q rejected: %s", argv, reply.Error)
	}
	return stream
}

type execResult struct {
	stdout, stderr string
	exit           protocol.ExecExit
}

// collect reads frames until the exit frame.
func collect(t *testing.T, stream net.Conn) execResult {
	t.Helper()
	var result execResult
	stream.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		frameType, payload, err := protocol.ReadFrame(stream)
		if err != nil {
			t.Fatalf("read frame: %v (stdout so far %q)", err, result.stdout)
		}
		switch frameType {
		case protocol.FrameStdout:
			result.stdout += string(payload)
		case protocol.FrameStderr:
			result.stderr += string(payload)
		case protocol.FrameExit:
			if err := json.Unmarshal(payload, &result.exit); err != nil {
				t.Fatal(err)
			}
			return result
		default:
			t.Fatalf("unexpected frame type %d", frameType)
		}
	}
}

// readPID reads the first stdout line, which the test command sets to $$.
func readPID(t *testing.T, stream net.Conn) int {
	t.Helper()
	stream.SetReadDeadline(time.Now().Add(10 * time.Second))
	frameType, payload, err := protocol.ReadFrame(stream)
	if err != nil || frameType != protocol.FrameStdout {
		t.Fatalf("read pid frame: type %d, %v", frameType, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(payload)))
	if err != nil {
		t.Fatalf("parse pid %q: %v", payload, err)
	}
	return pid
}

func waitGroupGone(t *testing.T, pgid int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for groupAlive(pgid) {
		if time.Now().After(deadline) {
			t.Fatalf("process group %d still alive after %v", pgid, within)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestExecStdinStdoutAndExitCode(t *testing.T) {
	f := newExecFixture(t)
	stream := f.mustStart("sh", "-c", "cat; echo err >&2; exit 7")
	protocol.WriteFrame(stream, protocol.FrameStdin, []byte("a\n"))
	protocol.WriteFrame(stream, protocol.FrameStdin, []byte("b\n"))
	protocol.WriteFrame(stream, protocol.FrameStdinEOF, nil)

	result := collect(t, stream)
	if result.stdout != "a\nb\n" || result.stderr != "err\n" || result.exit.Code != 7 || result.exit.Signal != "" {
		t.Fatalf("result = %+v", result)
	}
}

func TestExecEnvironmentAndWorkingDirectory(t *testing.T) {
	f := newExecFixture(t)
	if err := PublishEnvironment(map[string]string{"FOO": "published", "BAR": "kept"}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	stream, reply := f.start(protocol.ExecRequest{
		Version: 1, Argv: []string{"sh", "-c", `echo "$FOO $BAR"; pwd`}, Cwd: dir,
		Env: map[string]string{"FOO": "override"},
	})
	if !reply.OK {
		t.Fatalf("rejected: %s", reply.Error)
	}
	protocol.WriteFrame(stream, protocol.FrameStdinEOF, nil)
	if got := collect(t, stream).stdout; got != "override kept\n"+dir+"\n" {
		t.Fatalf("stdout = %q", got)
	}
}

func TestExecSignalFrame(t *testing.T) {
	f := newExecFixture(t)
	stream := f.mustStart("sleep", "100")
	protocol.WriteJSONFrame(stream, protocol.FrameSignal, protocol.ExecSignal{Signal: "TERM"})
	result := collect(t, stream)
	if result.exit.Signal != "TERM" || result.exit.Code != 143 {
		t.Fatalf("exit = %+v, want TERM/143", result.exit)
	}
}

func TestExecRejectsInvalidRequests(t *testing.T) {
	f := newExecFixture(t)

	_, reply := f.start(protocol.ExecRequest{Version: 1, Argv: []string{"dworm-no-such-command"}})
	if reply.OK || reply.ExitCode != 127 {
		t.Fatalf("missing command reply = %+v, want exit code 127", reply)
	}
	_, reply = f.start(protocol.ExecRequest{Version: 1})
	if reply.OK || !strings.Contains(reply.Error, "argv") {
		t.Fatalf("empty argv reply = %+v", reply)
	}
	_, reply = f.start(protocol.ExecRequest{Version: 1, Argv: []string{"true"}, Env: map[string]string{"BAD-NAME": "x"}})
	if reply.OK {
		t.Fatal("invalid environment accepted")
	}

	stream, err := f.h.HostMux.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	header := make([]byte, 5)
	header[0] = protocol.StreamTypeExec
	binary.BigEndian.PutUint32(header[1:], protocol.MaxExecHeaderSize+1)
	stream.Write(header)
	if err := protocol.ReadJSONMessage(stream, protocol.MaxExecHeaderSize, &reply); err != nil || reply.OK || !strings.Contains(reply.Error, "too large") {
		t.Fatalf("oversized header reply = %+v, %v", reply, err)
	}
}

func TestExecConcurrencyLimit(t *testing.T) {
	f := newExecFixture(t)
	f.server.execs.limit = 2
	f.mustStart("sleep", "100")
	f.mustStart("sleep", "100")
	_, reply := f.start(protocol.ExecRequest{Version: 1, Argv: []string{"true"}})
	if reply.OK || !strings.Contains(reply.Error, "too many concurrent execs") {
		t.Fatalf("reply over limit = %+v", reply)
	}
}

func TestExecConcurrentSessions(t *testing.T) {
	f := newExecFixture(t)
	var wg sync.WaitGroup
	for i := range 10 {
		stream := f.mustStart("cat")
		wg.Add(1)
		go func() {
			defer wg.Done()
			want := fmt.Sprintf("session %d\n", i)
			protocol.WriteFrame(stream, protocol.FrameStdin, []byte(want))
			protocol.WriteFrame(stream, protocol.FrameStdinEOF, nil)
			if got := collect(t, stream); got.stdout != want || got.exit.Code != 0 {
				t.Errorf("session %d result = %+v", i, got)
			}
		}()
	}
	wg.Wait()
}

func TestExecKillsProcessGroupOnDisconnect(t *testing.T) {
	for name, script := range map[string]string{
		"exits on TERM": `sleep 1000 & echo $$; wait`,
		"ignores TERM":  `trap "" TERM; sleep 1000 & echo $$; while :; do sleep 0.05; done`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newExecFixture(t)
			stream := f.mustStart("sh", "-c", script)
			pgid := readPID(t, stream)
			if !groupAlive(pgid) {
				t.Fatal("process group not running")
			}
			stream.Close()
			waitGroupGone(t, pgid, f.server.execs.grace+time.Second)
		})
	}
}

func TestExecKillsProcessGroupsOnShutdown(t *testing.T) {
	f := newExecFixture(t)
	stream := f.mustStart("sh", "-c", `trap "" TERM; echo $$; while :; do sleep 0.05; done`)
	pgid := readPID(t, stream)
	f.server.execs.closeAll()
	waitGroupGone(t, pgid, time.Second)

	_, reply := f.start(protocol.ExecRequest{Version: 1, Argv: []string{"true"}})
	if reply.OK {
		t.Fatal("exec accepted after shutdown")
	}
}

func (f *execFixture) startTTY(script string) net.Conn {
	f.t.Helper()
	stream, reply := f.start(protocol.ExecRequest{
		Version: 1, Argv: []string{"sh", "-c", script}, ID: "tty",
		TTY: &protocol.ExecTTY{Rows: 24, Cols: 80, Term: "xterm-test"},
	})
	if !reply.OK {
		f.t.Fatalf("tty exec rejected: %s", reply.Error)
	}
	return stream
}

// readUntil reads stdout frames until the output contains want.
func readUntil(t *testing.T, stream net.Conn, want string) string {
	t.Helper()
	stream.SetReadDeadline(time.Now().Add(10 * time.Second))
	var out string
	for !strings.Contains(out, want) {
		frameType, payload, err := protocol.ReadFrame(stream)
		if err != nil {
			t.Fatalf("waiting for %q: %v (output %q)", want, err, out)
		}
		if frameType != protocol.FrameStdout {
			t.Fatalf("unexpected frame type %d in TTY mode (output %q)", frameType, out)
		}
		out += string(payload)
	}
	return out
}

func waitSessionGone(t *testing.T, sid int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for sessionAlive(sid) {
		if time.Now().After(deadline) {
			t.Fatalf("session %d still alive after %v: %v", sid, within, sessionMembers(sid))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestExecTTYResizeAndTerm(t *testing.T) {
	f := newExecFixture(t)
	stream := f.startTTY(`stty size; read x; stty size; echo "term=$TERM"; test -t 0 && test -t 2 && echo tty-ok`)
	readUntil(t, stream, "24 80")
	protocol.WriteJSONFrame(stream, protocol.FrameResize, protocol.ExecResize{Rows: 40, Cols: 100})
	protocol.WriteFrame(stream, protocol.FrameStdin, []byte("\r"))
	result := collect(t, stream)
	for _, want := range []string{"40 100", "term=xterm-test", "tty-ok"} {
		if !strings.Contains(result.stdout, want) {
			t.Fatalf("output %q lacks %q", result.stdout, want)
		}
	}
	if result.stderr != "" || result.exit.Code != 0 {
		t.Fatalf("result = %+v; want no stderr frames and exit 0", result)
	}
}

func TestExecTTYCtrlCInterrupts(t *testing.T) {
	f := newExecFixture(t)
	stream := f.startTTY(`trap "echo got-INT; exit 7" INT; echo ready; while :; do sleep 0.05; done`)
	readUntil(t, stream, "ready")
	protocol.WriteFrame(stream, protocol.FrameStdin, []byte{0x03})
	result := collect(t, stream)
	if !strings.Contains(result.stdout, "got-INT") || result.exit.Code != 7 {
		t.Fatalf("result = %+v; want the INT trap and exit 7", result)
	}
}

func TestExecTTYDisconnectHangsUpSession(t *testing.T) {
	for name, script := range map[string]string{
		"hangup first":     `trap "echo hup > $MARKER; exit 0" HUP; echo "pid=$$."; while :; do sleep 0.05; done`,
		"ignores HUP+TERM": `trap "" HUP TERM; echo "pid=$$."; while :; do sleep 0.05; done`,
		// Job control puts the background job in its own process group of
		// the same session.
		"job control": `set -m; sleep 1000 & echo "pid=$$."; wait`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newExecFixture(t)
			marker := filepath.Join(t.TempDir(), "hup")
			t.Setenv("MARKER", marker)
			stream := f.startTTY(script)
			out := readUntil(t, stream, ".")
			pid, err := strconv.Atoi(out[strings.Index(out, "pid=")+4 : strings.Index(out, ".")])
			if err != nil {
				t.Fatalf("parse pid from %q: %v", out, err)
			}
			if !sessionAlive(pid) {
				t.Fatal("session not running")
			}
			stream.Close()
			waitSessionGone(t, pid, 2*f.server.execs.grace+2*time.Second)
			if name == "hangup first" {
				if data, _ := os.ReadFile(marker); strings.TrimSpace(string(data)) != "hup" {
					t.Fatalf("SIGHUP was not delivered first (marker %q)", data)
				}
			}
		})
	}
}
