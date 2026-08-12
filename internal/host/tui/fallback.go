package tui

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"golang.org/x/term"
)

// IsTerminal returns true if the file descriptor is a terminal
func IsTerminal(fd int) bool {
	return term.IsTerminal(fd)
}

// runFallback runs the shell without the TUI (fallback for non-TTY)
func runFallback(containerID, containerName, workDir string, envVars map[string]string, failureCh <-chan error) error {
	cmd := buildDockerCommand(containerID, workDir, envVars)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Handle signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	go func() {
		for sig := range sigCh {
			if cmd.Process != nil {
				cmd.Process.Signal(sig)
			}
		}
	}()

	if err := cmd.Start(); err != nil {
		return err
	}

	err, failureErr := waitForCommand(cmd, failureCh)
	if failureErr != nil {
		return failureErr
	}
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return fmt.Errorf("exit %d", exitErr.ExitCode())
		}
		return err
	}

	return nil
}

func buildDockerCommand(containerID, workDir string, envVars map[string]string) *exec.Cmd {
	args := []string{"exec", "-it"}

	if workDir != "" {
		args = append(args, "-w", workDir)
	}

	for key, value := range envVars {
		args = append(args, "-e", key+"="+value)
	}

	args = append(args, containerID, "/bin/bash")

	return exec.Command("docker", args...)
}
