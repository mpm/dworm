package tui

import (
	"os"

	"github.com/mpm/dworm/internal/host"
	"github.com/mpm/dworm/internal/protocol"
	"golang.org/x/term"
)

// IsTerminal returns true if the file descriptor is a terminal
func IsTerminal(fd int) bool {
	return term.IsTerminal(fd)
}

// runFallback runs the shell without the TUI (for non-TTY use): a plain
// framed exec with stdio passed through.
func runFallback(cfg Config) error {
	return host.ExecViaSocket(cfg.SocketPath, protocol.ExecRequest{Argv: cfg.Argv}, os.Stdin, os.Stdout, os.Stderr)
}
