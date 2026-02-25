package host

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"syscall"
)

// ExecShell opens an interactive shell in the container
func ExecShell(containerID string, workDir string, envVars map[string]string) error {
	return execInContainer(containerID, workDir, envVars, true, []string{"/bin/bash"})
}

// ExecCommand runs a command in the container and exits when it completes.
func ExecCommand(containerID string, workDir string, envVars map[string]string, command []string) error {
	if len(command) == 0 {
		return fmt.Errorf("no command specified")
	}

	return execInContainer(containerID, workDir, envVars, false, command)
}

func execInContainer(containerID string, workDir string, envVars map[string]string, interactive bool, command []string) error {
	args := buildDockerExecArgs(containerID, workDir, envVars, interactive, command)

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
		// Exit code from command is not an execution error.
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		return fmt.Errorf("docker exec failed: %w", err)
	}

	return nil
}

func buildDockerExecArgs(containerID string, workDir string, envVars map[string]string, interactive bool, command []string) []string {
	args := []string{"exec"}

	if interactive {
		args = append(args, "-it")
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
	args = append(args, command...)

	return args
}
