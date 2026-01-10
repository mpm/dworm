package protocol

import (
	"bytes"
	"io"
)

// CRWriter wraps an io.Writer and ensures each line starts with a carriage return.
// This fixes terminal output issues when multiple processes write to the same terminal,
// ensuring the cursor returns to column 0 before each log line.
type CRWriter struct {
	w io.Writer
}

// NewCRWriter creates a new CRWriter that wraps the given writer.
func NewCRWriter(w io.Writer) *CRWriter {
	return &CRWriter{w: w}
}

// Write implements io.Writer. It prepends \r to ensure cursor starts at column 0.
func (c *CRWriter) Write(p []byte) (n int, err error) {
	if len(p) == 0 {
		return 0, nil
	}

	// Prepend \r to reset cursor to column 0
	buf := make([]byte, 0, len(p)+bytes.Count(p, []byte{'\n'})+1)
	buf = append(buf, '\r')

	// Also add \r before each \n to handle multi-line output
	for i, b := range p {
		buf = append(buf, b)
		if b == '\n' && i < len(p)-1 {
			buf = append(buf, '\r')
		}
	}

	_, err = c.w.Write(buf)
	if err != nil {
		return 0, err
	}
	return len(p), nil
}
