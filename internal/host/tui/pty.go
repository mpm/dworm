package tui

import (
	"os"
	"os/exec"

	"github.com/creack/pty"
	"golang.org/x/term"
)

// StartWithPTY starts a command with a PTY and returns the PTY master
func StartWithPTY(cmd *exec.Cmd, rows, cols int) (*os.File, error) {
	size := &pty.Winsize{
		Rows: uint16(rows),
		Cols: uint16(cols),
	}
	return pty.StartWithSize(cmd, size)
}

// ResizePTY resizes the PTY to the specified dimensions
func ResizePTY(ptmx *os.File, rows, cols int) error {
	return pty.Setsize(ptmx, &pty.Winsize{
		Rows: uint16(rows),
		Cols: uint16(cols),
	})
}

// GetTerminalSize returns the current terminal size
func GetTerminalSize(fd int) (width, height int, err error) {
	return term.GetSize(fd)
}

// IsTerminal returns true if the file descriptor is a terminal
func IsTerminal(fd int) bool {
	return term.IsTerminal(fd)
}

// MakeRaw puts the terminal in raw mode and returns the previous state
func MakeRaw(fd int) (*term.State, error) {
	return term.MakeRaw(fd)
}

// Restore restores the terminal to the given state
func Restore(fd int, state *term.State) error {
	return term.Restore(fd, state)
}
