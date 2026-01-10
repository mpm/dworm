package host

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

// ExecShell opens an interactive shell in the container
func ExecShell(containerID string, workDir string, envVars map[string]string) error {
	// Determine shell to use
	shell := "/bin/bash"

	args := []string{"exec", "-it"}

	if workDir != "" {
		args = append(args, "-w", workDir)
	}

	// Add environment variables
	for key, value := range envVars {
		args = append(args, "-e", key+"="+value)
	}

	args = append(args, containerID, shell)

	cmd := exec.Command("docker", args...)
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

	if err := cmd.Run(); err != nil {
		// Exit code from shell is not an error
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		return fmt.Errorf("shell execution failed: %w", err)
	}

	return nil
}

// ExecCommand executes a command in the container and returns the output
func ExecCommand(containerID string, command []string) ([]byte, error) {
	args := append([]string{"exec", containerID}, command...)
	cmd := exec.Command("docker", args...)

	return cmd.CombinedOutput()
}
