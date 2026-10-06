package host

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestBuildDockerExecArgs(t *testing.T) {
	env := map[string]string{"B": "2", "A": "1"}
	tests := []struct {
		name  string
		tty   bool
		shell bool
		dir   string
		want  []string
	}{
		{name: "piped", want: []string{"exec", "-i", "-e", "A=1", "-e", "B=2", "cid"}},
		{name: "terminal", tty: true, want: []string{"exec", "-i", "-t", "-e", "A=1", "-e", "B=2", "cid"}},
		{name: "workdir", dir: "/work", want: []string{"exec", "-i", "-w", "/work", "-e", "A=1", "-e", "B=2", "cid"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildDockerExecArgs("cid", tt.dir, env, tt.tty, tt.shell, []string{"cat"})
			if !reflect.DeepEqual(got[:len(tt.want)], tt.want) {
				t.Fatalf("args = %q, want prefix %q", got, tt.want)
			}
			launcher := got[len(tt.want):]
			if launcher[len(launcher)-1] != "cat" || !contains(launcher, "--with-env") {
				t.Fatalf("launcher = %q, want --with-env ... cat", launcher)
			}
		})
	}
	shell := buildDockerExecArgs("cid", "", nil, true, true, []string{"/bin/bash"})
	if !contains(shell, "--shell") || contains(shell, "--with-env") {
		t.Fatalf("shell launcher = %q, want --shell", shell)
	}
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

// fakeDocker installs a docker executable running script and returns its dir.
func fakeDocker(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\n"+script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func TestRunDockerPassesStdioAndExitCode(t *testing.T) {
	fakeDocker(t, "cat\necho diagnostics >&2\nexit 7\n")

	var stdout, stderr bytes.Buffer
	err := runDocker([]string{"exec"}, strings.NewReader("a\nb\n"), &stdout, &stderr)
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 7 {
		t.Fatalf("err = %v, want exit code 7", err)
	}
	if stdout.String() != "a\nb\n" {
		t.Fatalf("stdout = %q, want stdin bytes only", stdout.String())
	}
	if stderr.String() != "diagnostics\n" {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunDockerSuccess(t *testing.T) {
	fakeDocker(t, "exit 0\n")
	if err := runDocker([]string{"exec"}, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("runDocker: %v", err)
	}
}
