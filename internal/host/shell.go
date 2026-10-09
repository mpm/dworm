package host

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"syscall"

	"github.com/mpm/dworm/internal/protocol"
	"golang.org/x/term"
)

// ExitError reports a command's non-zero exit status. It is not an execution
// failure: callers should exit with Code without printing an error.
type ExitError struct {
	Code int
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("exit status %d", e.Code)
}

// ExecCommand runs a command in the container and exits when it completes.
// Stdin is always attached; a TTY is allocated only when stdin and stdout are
// both terminals, so piped use gets a clean byte stream.
func ExecCommand(containerID string, workDir string, envVars map[string]string, command []string) error {
	return ExecCommandTTY(containerID, workDir, envVars, command, StdioIsTerminal())
}

// ExecCommandTTY runs plain docker exec with an explicit PTY choice. Docker
// owns local raw mode and resizing in this path.
func ExecCommandTTY(containerID string, workDir string, envVars map[string]string, command []string, tty bool) error {
	if len(command) == 0 {
		return fmt.Errorf("no command specified")
	}

	args := buildDockerExecArgs(containerID, workDir, envVars, tty, false, command)
	return runDocker(args, os.Stdin, os.Stdout, os.Stderr)
}

// StdioIsTerminal reports whether both stdin and stdout are terminals.
func StdioIsTerminal() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

// runDocker runs the docker CLI with the given stdio, forwarding SIGINT and
// SIGTERM. A non-zero exit status is returned as *ExitError.
func runDocker(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	cmd := exec.Command("docker", args...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	// Handle signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("docker exec failed: %w", err)
	}

	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case sig := <-sigCh:
				cmd.Process.Signal(sig)
			case <-done:
				return
			}
		}
	}()

	if err := cmd.Wait(); err != nil {
		// Exit code from command is not an execution error.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return &ExitError{Code: exitErr.ExitCode()}
		}
		return fmt.Errorf("docker exec failed: %w", err)
	}

	return nil
}

func buildDockerExecArgs(containerID string, workDir string, envVars map[string]string, tty bool, shell bool, command []string) []string {
	args := []string{"exec", "-i"}

	if tty {
		args = append(args, "-t")
	}

	if workDir != "" {
		args = append(args, "-w", workDir)
	}

	keys := make([]string, 0, len(envVars))
	for key := range envVars {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		args = append(args, "-e", key+"="+envVars[key])
	}

	args = append(args, containerID)
	args = append(args, protocol.EnvironmentCommand(envVars, shell, command)...)

	return args
}
