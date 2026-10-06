package protocol

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// Exec streams carry processes started through a running instance.
//
// On the bridge, the host opens a stream, writes StreamTypeExec, then a
// length-prefixed ExecRequest (WriteJSONMessage). The endpoint answers with a
// length-prefixed ExecReply. On success both sides then exchange frames.
//
// A frame is [1 byte type][4 byte big-endian length][payload].
const (
	FrameStdin    byte = 0x00 // client → process: stdin bytes
	FrameStdout   byte = 0x01 // process → client: stdout bytes
	FrameStderr   byte = 0x02 // process → client: stderr bytes
	FrameStdinEOF byte = 0x03 // client → process: close stdin (empty payload)
	FrameExit     byte = 0x04 // process → client: ExecExit JSON, last frame
	FrameSignal   byte = 0x05 // client → process: ExecSignal JSON
	FrameResize   byte = 0x06 // client → process: ExecResize JSON (TTY mode)
)

// Exec limits
const (
	ExecRequestVersion = 1
	MaxExecHeaderSize  = 64 * 1024       // max request header size
	MaxFrameSize       = 1024 * 1024     // max frame payload size
	MaxConcurrentExecs = 32              // per endpoint
	ExecKillGrace      = 5 * time.Second // SIGTERM → SIGKILL delay
)

// Exec modes for local socket clients.
const (
	ExecModeRaw    = "raw"
	ExecModeFramed = "framed"
)

// ExecRequest asks to run a process. Clients of the host socket send it as one
// JSON line; on the bridge the host adds ID and sends it length-prefixed.
type ExecRequest struct {
	Version int               `json:"version"`
	Argv    []string          `json:"argv"`
	Cwd     string            `json:"cwd,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Mode    string            `json:"mode,omitempty"`
	ID      string            `json:"id,omitempty"`
	TTY     *ExecTTY          `json:"tty,omitempty"` // run on a new PTY (framed mode only)
}

// ExecTTY asks for a PTY of the given size. Term, when set, becomes TERM.
// In TTY mode all output arrives as FrameStdout and FrameResize changes the
// size; Ctrl-C and similar keys travel as stdin bytes.
type ExecTTY struct {
	Rows int    `json:"rows"`
	Cols int    `json:"cols"`
	Term string `json:"term,omitempty"`
}

// ExecResize is the payload of FrameResize.
type ExecResize struct {
	Rows int `json:"rows"`
	Cols int `json:"cols"`
}

// ExecReply answers an ExecRequest. ExitCode is set when the process could not
// be started (127: not found, 126: not executable).
type ExecReply struct {
	OK       bool   `json:"ok"`
	ID       string `json:"id,omitempty"`
	Error    string `json:"error,omitempty"`
	Code     string `json:"code,omitempty"` // machine-readable error, e.g. "not_ready"
	ExitCode int    `json:"exit_code,omitempty"`
}

// ExecExit is the payload of FrameExit. A process killed by a signal reports
// Signal (e.g. "TERM") and Code 128+signal number. Error is set when the
// exit status is unknown, e.g. "bridge lost" (code 255) when the instance
// lost the endpoint running the process.
type ExecExit struct {
	Code   int    `json:"code"`
	Signal string `json:"signal,omitempty"`
	Error  string `json:"error,omitempty"`
}

// ExecErrorBridgeLost is ExecExit.Error for processes lost with the bridge.
const ExecErrorBridgeLost = "bridge lost"

// ExecSignal is the payload of FrameSignal. Signal is a name without the SIG
// prefix, e.g. "TERM", "INT", "HUP", "KILL".
type ExecSignal struct {
	Signal string `json:"signal"`
}

// WriteFrame writes one frame with a single Write call.
func WriteFrame(w io.Writer, frameType byte, payload []byte) error {
	if len(payload) > MaxFrameSize {
		return fmt.Errorf("frame too large: %d bytes", len(payload))
	}
	buf := make([]byte, 5+len(payload))
	buf[0] = frameType
	binary.BigEndian.PutUint32(buf[1:5], uint32(len(payload)))
	copy(buf[5:], payload)
	_, err := w.Write(buf)
	return err
}

// WriteJSONFrame writes a frame whose payload is v encoded as JSON.
func WriteJSONFrame(w io.Writer, frameType byte, v interface{}) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return WriteFrame(w, frameType, data)
}

// ReadFrame reads one frame.
func ReadFrame(r io.Reader) (byte, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, err
	}
	length := binary.BigEndian.Uint32(header[1:5])
	if length > MaxFrameSize {
		return 0, nil, fmt.Errorf("frame too large: %d bytes", length)
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, fmt.Errorf("read frame payload: %w", err)
	}
	return header[0], payload, nil
}

// WriteJSONMessage writes v as a 4-byte big-endian length followed by JSON.
func WriteJSONMessage(w io.Writer, v interface{}) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	buf := make([]byte, 4+len(data))
	binary.BigEndian.PutUint32(buf, uint32(len(data)))
	copy(buf[4:], data)
	_, err = w.Write(buf)
	return err
}

// ReadJSONMessage reads a message written by WriteJSONMessage into v.
func ReadJSONMessage(r io.Reader, maxSize int, v interface{}) error {
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return err
	}
	length := binary.BigEndian.Uint32(lenBuf[:])
	if int64(length) > int64(maxSize) {
		return fmt.Errorf("message too large: %d bytes", length)
	}
	data := make([]byte, length)
	if _, err := io.ReadFull(r, data); err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}
