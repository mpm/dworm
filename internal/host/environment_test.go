package host

import (
	"context"
	"io"
	"os/exec"
	"testing"
	"time"

	"github.com/mpm/dworm/internal/config"
)

func TestEnvironmentCommand(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("requires sh")
	}
	root := t.TempDir()
	t.Setenv("DWORM_TEST_TOKEN", "from-host")
	cfg := config.HostEnv{Command: []string{"sh", "-c", `printf 'TOKEN=%s\nROOT=%s\n' "$DWORM_TEST_TOKEN" "$PWD"; printf 'diagnostic\n' >&2`}}
	env, err := RunEnvironmentCommand(context.Background(), root, cfg, io.Discard)
	if err != nil || env["TOKEN"] != "from-host" || env["ROOT"] != root {
		t.Fatalf("%v %v", env, err)
	}
	for _, command := range []string{`printf 'TOKEN=partial\n'; exit 1`, `printf 'not an assignment\n'`, `yes A=too-much`} {
		cfg.Command[2] = command
		if _, err := RunEnvironmentCommand(context.Background(), root, cfg, io.Discard); err == nil {
			t.Errorf("accepted %q", command)
		}
	}
	cfg.Command[2], cfg.Timeout = "sleep 30 & wait", "50ms"
	start := time.Now()
	if _, err := RunEnvironmentCommand(context.Background(), root, cfg, io.Discard); err == nil {
		t.Fatal("expected timeout")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("timeout did not terminate command promptly")
	}
}

func TestRefreshStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	RefreshEnvironment(ctx, time.Millisecond, func() error { calls++; cancel(); return nil }, func(error) { t.Error("unexpected error") })
	if calls != 1 {
		t.Fatalf("calls = %d", calls)
	}
}
