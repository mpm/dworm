package endpoint

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mpm/dworm/internal/protocol"
	"github.com/mpm/dworm/internal/protocol/testutil"
)

// Subprocess entry point used by real Bash prompt hooks and launcher tests.
func TestEnvironmentHelper(t *testing.T) {
	root := os.Getenv("DWORM_TEST_ENV_DIR")
	if root == "" {
		return
	}
	environmentDir = root
	if os.Getenv("DWORM_TEST_LAUNCH") == "1" {
		if err := RunWithEnvironment(map[string]string{}, false, []string{"sh", "-c", `printf '%s' "$TOKEN"`}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	text, err := ShellEnvironment()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Print(text)
	os.Exit(0)
}

func TestEnvironmentAcrossIndependentShells(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("requires bash")
	}
	old := environmentDir
	environmentDir = t.TempDir()
	t.Cleanup(func() { environmentDir = old })
	if err := os.Chmod(environmentDir, 0700); err != nil {
		t.Fatal(err)
	}
	publish := func(values map[string]string) {
		t.Helper()
		if err := PublishEnvironment(values); err != nil {
			t.Fatal(err)
		}
	}
	publish(map[string]string{"TOKEN": "first", "RESTORE": "managed", "REMOVE": "managed"})
	info, err := os.Stat(filepath.Join(environmentDir, "environment.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("snapshot permissions: %v %v", info, err)
	}
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".bashrc"), []byte("PS1=''\nPROMPT_COMMAND='export USER_HOOK=preserved'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	rc := strings.ReplaceAll(bashRC, "/tmp/dworm_endpoint --shell-env", shellQuote(os.Args[0])+" -test.run=^TestEnvironmentHelper$")
	rcPath := filepath.Join(home, "dwormrc")
	if err := os.WriteFile(rcPath, []byte(rc), 0600); err != nil {
		t.Fatal(err)
	}
	type session struct {
		input  io.WriteCloser
		output *bufio.Reader
	}
	start := func(overrides map[string]string) session {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cmd := exec.CommandContext(ctx, "bash", "--noprofile", "--rcfile", rcPath, "-i")
		base, _ := json.Marshal(map[string]string{"RESTORE": "original"})
		pins, _ := json.Marshal(overrides)
		cmd.Env = append(os.Environ(), "HOME="+home, "DWORM_TEST_ENV_DIR="+environmentDir, "_DWORM_BASE="+string(base), "_DWORM_OVERRIDES="+string(pins), "_DWORM_KEYS={}", "RESTORE=original")
		input, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		output, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stderr = io.Discard
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { input.Close(); cancel(); cmd.Wait() })
		return session{input, bufio.NewReader(output)}
	}
	check := func(s session, want string) {
		t.Helper()
		// An empty input advances the prompt even if publication raced the last prompt.
		io.WriteString(s.input, "\nprintf 'DWORM_TEST_RESULT=%s|%s|%s|%s\\n' \"$TOKEN\" \"$RESTORE\" \"${REMOVE-unset}\" \"$USER_HOOK\"\n")
		line, err := s.output.ReadString('\n')
		for err == nil && !strings.HasPrefix(line, "DWORM_TEST_RESULT=") {
			line, err = s.output.ReadString('\n')
		}
		line = strings.TrimPrefix(line, "DWORM_TEST_RESULT=")
		if err != nil || strings.TrimSuffix(line, "\n") != want {
			t.Fatalf("got %q (%v), want %q", line, err, want)
		}
	}
	a, b, pinned := start(nil), start(nil), start(map[string]string{"TOKEN": "pinned"})
	check(a, "first|managed|managed|preserved")
	check(b, "first|managed|managed|preserved")
	publish(map[string]string{"TOKEN": "second ' $HOME $(false)"})
	check(a, "second ' $HOME $(false)|original|unset|preserved")
	check(b, "second ' $HOME $(false)|original|unset|preserved")
	check(pinned, "pinned|original|unset|preserved")
	cmd := exec.Command(os.Args[0], "-test.run=^TestEnvironmentHelper$")
	cmd.Env = append(os.Environ(), "DWORM_TEST_ENV_DIR="+environmentDir, "DWORM_TEST_LAUNCH=1")
	out, err := cmd.CombinedOutput()
	if err != nil || string(out) != "second ' $HOME $(false)" {
		t.Fatalf("fresh exec: %s, %v", out, err)
	}
	// Rejected updates leave the previous generation readable.
	if err := PublishEnvironment(map[string]string{"BAD-NAME": "invalid"}); err == nil {
		t.Fatal("accepted invalid snapshot")
	}
	check(a, "second ' $HOME $(false)|original|unset|preserved")
}

func TestEnvironmentControlUpdates(t *testing.T) {
	old := environmentDir
	environmentDir = t.TempDir()
	t.Cleanup(func() { environmentDir = old })
	if err := os.Chmod(environmentDir, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DWORM_TEST_MANAGED", "container-default")
	h, err := testutil.NewTestHarness()
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	s := NewServer()
	s.mux = h.EndpointMux
	done := make(chan error, 1)
	go func() {
		if err := s.waitForInit(); err != nil {
			done <- err
			return
		}
		done <- s.handleControlMessages()
	}()
	defer func() {
		h.Close()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, io.EOF) {
				t.Error(err)
			}
		case <-time.After(time.Second):
			t.Error("endpoint did not stop")
		}
	}()
	if err := h.HostMux.SendControl(protocol.TypeInit, &protocol.InitMessage{EnvVars: map[string]string{"DWORM_TEST_MANAGED": "initial"}}); err != nil {
		t.Fatal(err)
	}
	kind, _, err := h.HostMux.RecvControl()
	if err != nil || kind != protocol.TypeEnvironmentReady {
		t.Fatalf("ack: %s, %v", kind, err)
	}
	if env, err := readEnvironment(); err != nil || env["DWORM_TEST_MANAGED"] != "initial" {
		t.Fatalf("initial snapshot: %v, %v", env, err)
	}
	for _, next := range []map[string]string{{"DWORM_TEST_MANAGED": "refreshed"}, {}} {
		if err := h.HostMux.SendControl(protocol.TypeEnvironment, next); err != nil {
			t.Fatal(err)
		}
		// Ordered control processing makes pong a publication barrier.
		if err := h.HostMux.SendControl(protocol.TypePing, nil); err != nil {
			t.Fatal(err)
		}
		kind, _, err := h.HostMux.RecvControl()
		if err != nil || kind != protocol.TypePong {
			t.Fatalf("pong: %s, %v", kind, err)
		}
		env, err := readEnvironment()
		if err != nil || env["DWORM_TEST_MANAGED"] != next["DWORM_TEST_MANAGED"] {
			t.Fatalf("snapshot: %v, %v", env, err)
		}
	}
	if got := os.Getenv("DWORM_TEST_MANAGED"); got != "container-default" {
		t.Fatalf("removed variable = %q", got)
	}
}
