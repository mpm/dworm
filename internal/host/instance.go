package host

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// ErrAlreadyRunning is returned when another `dworm up` holds the workspace lock.
var ErrAlreadyRunning = errors.New("dworm up is already running for this workspace")

// InstancePaths are the per-workspace runtime files of a `dworm up` process.
// They are named after a short hash of the absolute workspace path, which keeps
// the socket path well below the 108-byte unix socket limit.
type InstancePaths struct {
	Dir    string
	Lock   string
	State  string
	Socket string
}

// RuntimeDir returns dworm's private runtime directory:
// $XDG_RUNTIME_DIR/dworm, or /tmp/dworm-$UID when XDG_RUNTIME_DIR is unset.
func RuntimeDir() string {
	if base := os.Getenv("XDG_RUNTIME_DIR"); base != "" {
		return filepath.Join(base, "dworm")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("dworm-%d", os.Getuid()))
}

// InstancePathsFor returns the runtime file paths for a workspace.
func InstancePathsFor(workspacePath string) InstancePaths {
	sum := sha256.Sum256([]byte(workspacePath))
	name := hex.EncodeToString(sum[:])[:12]
	dir := RuntimeDir()
	return InstancePaths{
		Dir:    dir,
		Lock:   filepath.Join(dir, name+".lock"),
		State:  filepath.Join(dir, name+".json"),
		Socket: filepath.Join(dir, name+".sock"),
	}
}

func ensureRuntimeDir(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create runtime directory: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("runtime directory %s is not a directory", dir)
	}
	// Only the owner can change the mode, so this also rejects a directory
	// pre-created by another user in a shared /tmp.
	if err := os.Chmod(dir, 0700); err != nil {
		return fmt.Errorf("secure runtime directory %s: %w", dir, err)
	}
	return nil
}

// InstanceLock is the exclusive per-workspace lock held by `dworm up`. The
// kernel releases it when the process exits, so a crash leaves no stale lock.
type InstanceLock struct {
	Paths InstancePaths
	file  *os.File
}

// AcquireInstanceLock takes the workspace lock without blocking. It returns
// ErrAlreadyRunning when another process holds it.
func AcquireInstanceLock(workspacePath string) (*InstanceLock, error) {
	paths := InstancePathsFor(workspacePath)
	if err := ensureRuntimeDir(paths.Dir); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(paths.Lock, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, fmt.Errorf("open instance lock: %w", err)
	}
	// Status probes hold a shared lock for an instant; retry briefly so a
	// concurrent `dworm status` cannot make `dworm up` fail.
	for attempt := 0; ; attempt++ {
		err = lockFile(file, true)
		if err == nil {
			return &InstanceLock{Paths: paths, file: file}, nil
		}
		if !errors.Is(err, errLocked) || attempt >= 5 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	file.Close()
	if errors.Is(err, errLocked) {
		return nil, ErrAlreadyRunning
	}
	return nil, fmt.Errorf("lock instance: %w", err)
}

// Release removes the state file and releases the lock. The lock file itself
// stays: deleting a flock file would race with a concurrent acquirer.
func (l *InstanceLock) Release() {
	os.Remove(l.Paths.State)
	l.file.Close()
}

// InstanceRunning reports whether a `dworm up` currently holds the lock.
func InstanceRunning(paths InstancePaths) (bool, error) {
	file, err := os.Open(paths.Lock)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer file.Close()
	err = lockFile(file, false)
	if errors.Is(err, errLocked) {
		return true, nil
	}
	return false, err
}

// StatePort is a forwarded port: Port inside the container, reachable on the
// host at Address:LocalPort.
type StatePort struct {
	Port      int    `json:"port"`
	Address   string `json:"address"`
	LocalPort int    `json:"local_port"`
}

// InstanceState is the state file written by a running `dworm up`.
type InstanceState struct {
	PID               int         `json:"pid"`
	WorkspacePath     string      `json:"workspace_path"`
	ContainerID       string      `json:"container_id"`
	ContainerName     string      `json:"container_name"`
	RemoteUser        string      `json:"remote_user"`
	WorkspaceFolder   string      `json:"workspace_folder"`
	StartedAt         time.Time   `json:"started_at"`
	EndpointConnected bool        `json:"endpoint_connected"`
	Ports             []StatePort `json:"ports"`
	ExecSocket        string      `json:"exec_socket,omitempty"`
}

// StateFile serializes updates to an instance's state file.
type StateFile struct {
	path  string
	mu    sync.Mutex
	state InstanceState
}

// NewStateFile writes the initial state and returns a handle for updates.
func NewStateFile(path string, initial InstanceState) (*StateFile, error) {
	if initial.Ports == nil {
		initial.Ports = []StatePort{}
	}
	s := &StateFile{path: path, state: initial}
	return s, s.Update(func(*InstanceState) {})
}

// Update applies change and atomically rewrites the state file.
func (s *StateFile) Update(change func(*InstanceState)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	change(&s.state)
	sort.Slice(s.state.Ports, func(i, j int) bool { return s.state.Ports[i].Port < s.state.Ports[j].Port })
	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.path, append(data, '\n'))
}

// ReadInstanceState reads a state file.
func ReadInstanceState(path string) (*InstanceState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var state InstanceState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parse state file %s: %w", path, err)
	}
	return &state, nil
}

func writeFileAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
