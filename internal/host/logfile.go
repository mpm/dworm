package host

import (
	"fmt"
	"os"
	"sync"
)

// MaxInstanceLogSize is the size at which a detached instance's log file is
// rotated to <log>.1.
const MaxInstanceLogSize = 5 * 1024 * 1024

// RotatingLog appends to a file and, once it would exceed max bytes, renames
// it to path+".1" (replacing an older one) and starts a new file. With
// redirectStdio, the process's stdout and stderr follow the current file, so
// panics and other direct output land in it too.
type RotatingLog struct {
	path     string
	max      int64
	redirect bool

	mu   sync.Mutex
	file *os.File
	size int64
}

// OpenRotatingLog opens path for appending, rotating it first if it is
// already too large.
func OpenRotatingLog(path string, max int64, redirectStdio bool) (*RotatingLog, error) {
	l := &RotatingLog{path: path, max: max, redirect: redirectStdio}
	if err := l.open(); err != nil {
		return nil, err
	}
	if l.size >= max {
		if err := l.rotate(); err != nil {
			l.file.Close()
			return nil, err
		}
	}
	return l, nil
}

func (l *RotatingLog) open() error {
	file, err := os.OpenFile(l.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return err
	}
	l.file, l.size = file, info.Size()
	if l.redirect {
		if err := redirectStdio(file); err != nil {
			return fmt.Errorf("redirect output to log file: %w", err)
		}
	}
	return nil
}

func (l *RotatingLog) rotate() error {
	l.file.Close()
	if err := os.Rename(l.path, l.path+".1"); err != nil && !os.IsNotExist(err) {
		return err
	}
	return l.open()
}

// Write implements io.Writer.
func (l *RotatingLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.size > 0 && l.size+int64(len(p)) > l.max {
		if err := l.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := l.file.Write(p)
	l.size += int64(n)
	return n, err
}

// Close closes the current file.
func (l *RotatingLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.file.Close()
}
