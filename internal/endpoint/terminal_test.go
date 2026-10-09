package endpoint

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mpm/dworm/internal/protocol"
)

func TestTerminalNameContainerTerminfo(t *testing.T) {
	root := t.TempDir()
	// Synthetic entries let this test work without ncurses utilities or a
	// particular host terminfo installation. Cover both directory conventions.
	for _, entry := range []string{"64/dworm-custom-terminal", "x/xterm-256color"} {
		path := filepath.Join(root, entry)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("entry"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	env := map[string]string{"TERMINFO": root, "PATH": ""}
	for _, tt := range []struct{ name, want string }{
		{"dworm-custom-terminal", "dworm-custom-terminal"},
		{"dworm-missing-terminal", "xterm-256color"},
		{"", "xterm-256color"},
	} {
		if got := terminalName(tt.name, env, ""); got != tt.want {
			t.Errorf("terminalName(%q) = %q, want %q", tt.name, got, tt.want)
		}
	}
	// TERMINFO_DIRS is part of the child's merged environment too.
	if got := terminalName("dworm-custom-terminal", map[string]string{"TERMINFO_DIRS": root}, ""); got != "dworm-custom-terminal" {
		t.Fatalf("custom TERMINFO_DIRS ignored: %q", got)
	}
}

func TestExecTTYFallsBackForMissingTerminfo(t *testing.T) {
	f := newExecFixture(t)
	stream, reply := f.start(protocol.ExecRequest{
		Argv: []string{"sh", "-c", `echo "term=$TERM"`},
		TTY:  &protocol.ExecTTY{Rows: 24, Cols: 80, Term: "dworm-missing-terminal"},
	})
	if !reply.OK {
		t.Fatal(reply.Error)
	}
	result := collect(t, stream)
	if result.exit.Code != 0 || result.stdout != "term=xterm-256color\r\n" {
		t.Fatalf("missing terminfo fallback: %+v", result)
	}
}
