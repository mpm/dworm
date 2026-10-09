package endpoint

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// terminalName checks the container's terminfo, rather than the host's. In
// particular, minimal images often know xterm-256color but not tmux-256color.
func terminalName(name string, env map[string]string, dir string) string {
	if name == "" {
		name = "xterm-256color"
	}
	if hasTerminfo(name, env, dir) {
		return name
	}
	for _, fallback := range []string{"xterm-256color", "xterm", "vt100"} {
		if hasTerminfo(fallback, env, dir) {
			return fallback
		}
	}
	// Even images with no terminfo can run terminal-aware applications that
	// implement their own ANSI support.
	return "xterm-256color"
}

func hasTerminfo(name string, env map[string]string, dir string) bool {
	if name == "" || strings.ContainsAny(name, "/\\\x00") {
		return false
	}
	dirs := []string{env["TERMINFO"]}
	if home := env["HOME"]; home != "" {
		dirs = append(dirs, filepath.Join(home, ".terminfo"))
	}
	dirs = append(dirs, filepath.SplitList(env["TERMINFO_DIRS"])...)
	dirs = append(dirs, "/etc/terminfo", "/lib/terminfo", "/usr/share/terminfo", "/usr/share/lib/terminfo")
	for _, root := range dirs {
		if root == "" {
			continue
		}
		for _, prefix := range []string{name[:1], fmt.Sprintf("%x", name[0])} {
			if info, err := os.Stat(filepath.Join(root, prefix, name)); err == nil && info.Mode().IsRegular() {
				return true
			}
		}
	}
	// infocmp also understands hashed databases and compiled aliases. Use the
	// command's environment and a deadline so terminal setup cannot hang.
	path, err := lookPathIn("infocmp", env["PATH"], dir)
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "-x", "--", name)
	cmd.Dir = dir
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	return cmd.Run() == nil
}
