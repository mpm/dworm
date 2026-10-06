package endpoint

import (
	"bytes"
	"io"
	"sync"

	"github.com/mpm/dworm/internal/protocol"
)

// controlLogWriter sends log lines to the host as TypeLog control messages
// once attached; before that, and whenever its queue is full or sending
// fails, lines go to stderr (which the host also collects). Sending is
// asynchronous, so logging never blocks on the bridge.
type controlLogWriter struct {
	stderr io.Writer

	mu      sync.Mutex
	queue   chan protocol.LogMessage
	partial []byte
}

func newControlLogWriter(stderr io.Writer) *controlLogWriter {
	return &controlLogWriter{stderr: stderr}
}

// attach starts sending over the control channel with send.
func (w *controlLogWriter) attach(send func(string, interface{}) error) {
	queue := make(chan protocol.LogMessage, 256)
	w.mu.Lock()
	w.queue = queue
	w.mu.Unlock()
	go func() {
		for msg := range queue {
			if send(protocol.TypeLog, &msg) != nil {
				io.WriteString(w.stderr, msg.Message+"\n")
			}
		}
	}()
}

// detach returns to stderr, e.g. when the bridge closes.
func (w *controlLogWriter) detach() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.queue != nil {
		close(w.queue)
		w.queue = nil
	}
}

func (w *controlLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.partial = append(w.partial, p...)
	for {
		i := bytes.IndexByte(w.partial, '\n')
		if i < 0 {
			break
		}
		w.emitLocked(string(w.partial[:i]))
		w.partial = w.partial[i+1:]
	}
	return len(p), nil
}

func (w *controlLogWriter) emitLocked(line string) {
	if w.queue != nil {
		select {
		case w.queue <- protocol.LogMessage{Level: protocol.InferLogLevel(line), Message: line}:
			return
		default:
		}
	}
	io.WriteString(w.stderr, line+"\n")
}
