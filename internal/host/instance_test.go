//go:build unix

package host

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestInstanceLockIsExclusivePerWorkspace(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	first, err := AcquireInstanceLock("/projects/app")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	info, err := os.Stat(first.Paths.Dir)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("runtime dir mode = %v, %v; want 0700", info.Mode().Perm(), err)
	}
	if _, err := AcquireInstanceLock("/projects/app"); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second acquire error = %v, want ErrAlreadyRunning", err)
	}
	other, err := AcquireInstanceLock("/projects/other")
	if err != nil {
		t.Fatalf("other workspace: %v", err)
	}
	other.Release()

	if running, err := InstanceRunning(first.Paths); err != nil || !running {
		t.Fatalf("InstanceRunning = %v, %v; want true", running, err)
	}
	// A probe must not take the lock away from the holder.
	if _, err := AcquireInstanceLock("/projects/app"); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("acquire after probe = %v, want ErrAlreadyRunning", err)
	}
	first.Release()
	if running, err := InstanceRunning(first.Paths); err != nil || running {
		t.Fatalf("InstanceRunning after release = %v, %v; want false", running, err)
	}
	again, err := AcquireInstanceLock("/projects/app")
	if err != nil {
		t.Fatalf("reacquire: %v", err)
	}
	again.Release()
}

func TestInstanceRunningWithoutLockFile(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	if running, err := InstanceRunning(InstancePathsFor("/nowhere")); err != nil || running {
		t.Fatalf("InstanceRunning = %v, %v; want false, nil", running, err)
	}
}

func TestInstancePathsFallbackAndLength(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	paths := InstancePathsFor("/home/user/" + strings.Repeat("deep/", 40) + "project")
	if !strings.Contains(paths.Dir, "dworm-") {
		t.Fatalf("fallback dir = %q", paths.Dir)
	}
	if len(paths.Socket) >= 108 {
		t.Fatalf("socket path too long: %q", paths.Socket)
	}
	if InstancePathsFor("/a") == InstancePathsFor("/b") {
		t.Fatal("different workspaces share paths")
	}
}

func TestStateFileUpdatesAtomically(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	lock, err := AcquireInstanceLock("/projects/app")
	if err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	state, err := NewStateFile(lock.Paths.State, InstanceState{PID: 42, WorkspacePath: "/projects/app", StartedAt: started})
	if err != nil {
		t.Fatalf("write state: %v", err)
	}
	if err := state.Update(func(s *InstanceState) {
		s.EndpointConnected = true
		s.Ports = []StatePort{{Port: 8080, Address: "127.0.0.1", LocalPort: 8080}, {Port: 3000, Address: "127.0.0.1", LocalPort: 3001}}
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err := ReadInstanceState(lock.Paths.State)
	if err != nil {
		t.Fatal(err)
	}
	if got.PID != 42 || !got.EndpointConnected || !got.StartedAt.Equal(started) || len(got.Ports) != 2 || got.Ports[0].Port != 3000 {
		t.Fatalf("state = %+v", got)
	}
	entries, _ := os.ReadDir(lock.Paths.Dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".state-") {
			t.Fatalf("temporary file left behind: %s", e.Name())
		}
	}
	lock.Release()
	if _, err := os.Stat(lock.Paths.State); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state file remains after release: %v", err)
	}
}
