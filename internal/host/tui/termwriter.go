package tui

import (
	"io"
	"sync"
)

// TermWriter provides synchronized access to a terminal writer.
// It ensures that multi-byte ANSI sequences can be written atomically
// without interleaving with other writes.
type TermWriter struct {
	mu  sync.Mutex
	out io.Writer
}

// NewTermWriter creates a new synchronized terminal writer
func NewTermWriter(out io.Writer) *TermWriter {
	return &TermWriter{out: out}
}

// Write implements io.Writer with mutex protection
func (w *TermWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.out.Write(p)
}

// WriteString writes a string atomically
func (w *TermWriter) WriteString(s string) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return io.WriteString(w.out, s)
}

// Atomic executes a function with exclusive access to the underlying writer.
// Use this for multi-step ANSI sequences that must not be interleaved.
func (w *TermWriter) Atomic(fn func(io.Writer)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	fn(w.out)
}
