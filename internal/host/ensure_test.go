//go:build linux

package host

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// ensureEnv prepares Ensure to start the test binary as a fake instance.
func ensureEnv(t *testing.T, version string) EnsureOptions {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("DWORM_FAKE_INSTANCE", "1")
	t.Setenv("DWORM_FAKE_VERSION", version)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	t.Cleanup(func() { StopInstance(workspace, 10*time.Second) })
	return EnsureOptions{WorkspacePath: workspace, Executable: executable, Timeout: 20 * time.Second, Version: version}
}

func TestEnsureConcurrentClientsShareOneInstance(t *testing.T) {
	opts := ensureEnv(t, "vtest")
	const clients = 4
	states := make([]*InstanceState, clients)
	errs := make([]error, clients)
	var wg sync.WaitGroup
	for n := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			states[n], errs[n] = Ensure(context.Background(), opts)
		}()
	}
	wg.Wait()
	for n := range clients {
		if errs[n] != nil {
			t.Fatalf("client %d: %v", n, errs[n])
		}
		if states[n].State != StateReady || states[n].PID != states[0].PID || states[n].PID == os.Getpid() {
			t.Fatalf("client %d state = %+v, client 0 pid %d", n, states[n], states[0].PID)
		}
	}
	if states[0].Mode != ModeDetached {
		t.Fatalf("mode = %q", states[0].Mode)
	}

	// A further ensure returns the running instance immediately.
	start := time.Now()
	again, err := Ensure(context.Background(), opts)
	if err != nil || again.PID != states[0].PID || time.Since(start) > time.Second {
		t.Fatalf("second ensure = %+v, %v after %v", again, err, time.Since(start))
	}

	stopped, err := StopInstance(opts.WorkspacePath, 10*time.Second)
	if !stopped || err != nil {
		log, _ := os.ReadFile(InstancePathsFor(opts.WorkspacePath).Log)
		t.Fatalf("StopInstance = %v, %v\n%s", stopped, err, log)
	}
	if running, _ := InstanceRunning(InstancePathsFor(opts.WorkspacePath)); running {
		t.Fatal("instance still holds the lock after stop")
	}
	log, _ := os.ReadFile(InstancePathsFor(opts.WorkspacePath).Log)
	if !strings.Contains(string(log), "Instance started") {
		t.Fatalf("instance log = %q", log)
	}
}

func TestEnsureRejectsOtherVersion(t *testing.T) {
	opts := ensureEnv(t, "v0.8.0")
	if _, err := Ensure(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	opts.Version = "v0.9.0"
	_, err := Ensure(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "instance runs v0.8.0, this is v0.9.0; run `dworm stop`") {
		t.Fatalf("err = %v", err)
	}
}

func TestEnsureWarnsAboutIgnoredEnv(t *testing.T) {
	opts := ensureEnv(t, "vtest")
	if _, err := Ensure(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	var warnings strings.Builder
	opts.Env = map[string]string{"FOO": "bar"}
	opts.Warnings = &warnings
	if _, err := Ensure(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(warnings.String(), "dworm stop") {
		t.Fatalf("warnings = %q", warnings.String())
	}
}

func TestEnsureNoStartRequiresRunningContainer(t *testing.T) {
	opts := ensureEnv(t, "vtest")
	fakeDocker(t, "exit 0\n") // no container
	opts.NoStart = true
	if _, err := Ensure(context.Background(), opts); err != ErrContainerNotRunning {
		t.Fatalf("err = %v, want ErrContainerNotRunning", err)
	}
}
