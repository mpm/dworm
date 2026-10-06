package protocol

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, FrameStdout, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := WriteJSONFrame(&buf, FrameExit, ExecExit{Code: 143, Signal: "TERM"}); err != nil {
		t.Fatal(err)
	}
	if err := WriteFrame(&buf, FrameStdinEOF, nil); err != nil {
		t.Fatal(err)
	}
	if got := buf.Bytes()[:5]; !bytes.Equal(got, []byte{FrameStdout, 0, 0, 0, 5}) {
		t.Fatalf("frame header = %v", got)
	}
	for _, want := range []struct {
		frameType byte
		payload   string
	}{{FrameStdout, "hello"}, {FrameExit, `{"code":143,"signal":"TERM"}`}, {FrameStdinEOF, ""}} {
		frameType, payload, err := ReadFrame(&buf)
		if err != nil || frameType != want.frameType || string(payload) != want.payload {
			t.Fatalf("frame = %d %q, %v; want %d %q", frameType, payload, err, want.frameType, want.payload)
		}
	}
}

func TestFrameSizeLimits(t *testing.T) {
	if err := WriteFrame(&bytes.Buffer{}, FrameStdin, make([]byte, MaxFrameSize+1)); err == nil {
		t.Fatal("oversized frame written")
	}
	header := []byte{FrameStdin, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(header[1:], MaxFrameSize+1)
	if _, _, err := ReadFrame(bytes.NewReader(header)); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("oversized frame read: %v", err)
	}
}

func TestJSONMessageRoundTripAndLimit(t *testing.T) {
	var buf bytes.Buffer
	req := ExecRequest{Version: 1, Argv: []string{"opencode", "acp"}, Env: map[string]string{"A": "b"}, ID: "abc"}
	if err := WriteJSONMessage(&buf, req); err != nil {
		t.Fatal(err)
	}
	var got ExecRequest
	if err := ReadJSONMessage(bytes.NewReader(buf.Bytes()), MaxExecHeaderSize, &got); err != nil || got.ID != "abc" || got.Argv[1] != "acp" {
		t.Fatalf("got %+v, %v", got, err)
	}
	if err := ReadJSONMessage(bytes.NewReader(buf.Bytes()), 4, &got); err == nil {
		t.Fatal("message over limit accepted")
	}
}
