package tui

import (
	"strings"
	"sync"
)

const defaultLogBufferSize = 100

// LogBuffer is a thread-safe ring buffer that implements io.Writer.
// It stores the last N log lines and sends notifications when new lines arrive.
type LogBuffer struct {
	mu       sync.Mutex
	lines    []string
	size     int
	pos      int
	full     bool
	partial  string // incomplete line (no trailing newline yet)
	updateCh chan struct{}
}

// NewLogBuffer creates a new LogBuffer that stores up to size lines.
// The returned channel receives a notification whenever new lines are added.
func NewLogBuffer(size int) (*LogBuffer, <-chan struct{}) {
	if size <= 0 {
		size = defaultLogBufferSize
	}
	ch := make(chan struct{}, 1)
	return &LogBuffer{
		lines:    make([]string, size),
		size:     size,
		updateCh: ch,
	}, ch
}

// Write implements io.Writer. It splits input into lines and stores them.
func (b *LogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	text := b.partial + string(p)
	b.partial = ""

	for {
		idx := strings.IndexByte(text, '\n')
		if idx < 0 {
			// No more newlines — save as partial
			b.partial = text
			break
		}

		line := strings.TrimRight(text[:idx], "\r")
		b.addLine(line)
		text = text[idx+1:]
	}

	// Notify listener (non-blocking)
	select {
	case b.updateCh <- struct{}{}:
	default:
	}

	return len(p), nil
}

// Lines returns a copy of all stored log lines in order (oldest first).
func (b *LogBuffer) Lines() []string {
	b.mu.Lock()
	defer b.mu.Unlock()

	if !b.full {
		result := make([]string, b.pos)
		copy(result, b.lines[:b.pos])
		return result
	}

	result := make([]string, b.size)
	copy(result, b.lines[b.pos:])
	copy(result[b.size-b.pos:], b.lines[:b.pos])
	return result
}

func (b *LogBuffer) addLine(line string) {
	b.lines[b.pos] = line
	b.pos++
	if b.pos >= b.size {
		b.pos = 0
		b.full = true
	}
}
