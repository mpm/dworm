package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/mpm/dworm/internal/host"
	"github.com/mpm/dworm/internal/protocol"
	"github.com/spf13/cobra"
)

var (
	envVars    []string
	daemonMode bool
	logger     = log.New(os.Stderr, "", log.LstdFlags)
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "dworm",
		Short: "Development Wormhole - seamless devcontainer bridging",
		Long: `dworm wraps the devcontainer CLI to provide seamless development
environment bridging between your host machine and containerized development
environments.`,
	}

	// Global flags
	rootCmd.PersistentFlags().StringArrayVarP(&envVars, "env", "e", nil, "Environment variables to inject (KEY=VALUE)")

	// Up command
	upCmd := &cobra.Command{
		Use:   "up [path]",
		Short: "Start container, inject endpoint, establish tunnel",
		Args:  cobra.MaximumNArgs(1),
		RunE:  runUp,
	}
	upCmd.Flags().BoolVar(&daemonMode, "daemon", false, "Run in daemon mode (no shell)")
	rootCmd.AddCommand(upCmd)

	// Down command
	downCmd := &cobra.Command{
		Use:   "down [path]",
		Short: "Stop the devcontainer",
		Args:  cobra.MaximumNArgs(1),
		RunE:  runDown,
	}
	rootCmd.AddCommand(downCmd)

	// Shell command
	shellCmd := &cobra.Command{
		Use:   "shell [path]",
		Short: "Open a shell in the container",
		Args:  cobra.MaximumNArgs(1),
		RunE:  runShell,
	}
	rootCmd.AddCommand(shellCmd)

	// Status command
	statusCmd := &cobra.Command{
		Use:   "status [path]",
		Short: "Show forwarded ports and active configuration",
		Args:  cobra.MaximumNArgs(1),
		RunE:  runStatus,
	}
	rootCmd.AddCommand(statusCmd)

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func getWorkspacePath(args []string) (string, error) {
	path := "."
	if len(args) > 0 {
		path = args[0]
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("failed to get absolute path: %w", err)
	}

	return absPath, nil
}

func parseEnvVars() map[string]string {
	result := make(map[string]string)
	for _, env := range envVars {
		parts := strings.SplitN(env, "=", 2)
		if len(parts) == 2 {
			result[parts[0]] = parts[1]
		} else if len(parts) == 1 {
			// If no value, get from current environment
			if val, exists := os.LookupEnv(parts[0]); exists {
				result[parts[0]] = val
			}
		}
	}
	return result
}

func runUp(cmd *cobra.Command, args []string) error {
	workspacePath, err := getWorkspacePath(args)
	if err != nil {
		return err
	}

	logger.Printf("Starting devcontainer at %s...", workspacePath)

	// Start container
	containerInfo, err := host.DevcontainerUp(workspacePath)
	if err != nil {
		return fmt.Errorf("failed to start devcontainer: %w", err)
	}

	logger.Printf("Container started: %s", containerInfo.ContainerName)

	// Find endpoint binary
	endpointPath, err := host.GetEndpointBinaryPath()
	if err != nil {
		return fmt.Errorf("endpoint binary not found: %w", err)
	}

	// Create endpoint manager
	endpoint := host.NewEndpointManager(containerInfo.ContainerID)

	// Inject and start endpoint
	if err := endpoint.InjectAndStart(endpointPath); err != nil {
		return fmt.Errorf("failed to start endpoint: %w", err)
	}

	// Send init message
	envs := parseEnvVars()
	if err := endpoint.SendInit(envs); err != nil {
		endpoint.Close()
		return fmt.Errorf("failed to send init: %w", err)
	}

	logger.Printf("Endpoint initialized with %d env vars", len(envs))

	// Create tunnel manager
	tunnels := host.NewTunnelManager(endpoint)

	// Handle shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Handle control messages
	go func() {
		for {
			msgType, data, err := endpoint.RecvControl()
			if err != nil {
				logger.Printf("Control channel closed: %v", err)
				return
			}

			switch msgType {
			case protocol.TypePortUpdate:
				portMsg, err := protocol.DecodePortUpdate(data)
				if err != nil {
					logger.Printf("Failed to decode port update: %v", err)
					continue
				}
				logger.Printf("Port update: %v", portMsg.Ports)
				tunnels.UpdatePorts(portMsg.Ports)

				// Print forwarded ports
				forwarded := tunnels.GetForwardedPorts()
				for containerPort, localPort := range forwarded {
					fmt.Printf("Forwarding localhost:%d -> container:%d\n", localPort, containerPort)
				}
			}
		}
	}()

	if daemonMode {
		logger.Printf("Running in daemon mode. Press Ctrl+C to stop.")
		<-sigCh
		logger.Printf("Shutting down...")
	} else {
		// Give some time for initial port scan
		// Then open shell
		logger.Printf("Opening shell...")

		// Run shell in a goroutine so we can handle signals
		shellDone := make(chan error, 1)
		go func() {
			shellDone <- host.ExecShell(containerInfo.ContainerID, containerInfo.WorkspaceDir)
		}()

		select {
		case <-sigCh:
			logger.Printf("Interrupted, closing...")
		case err := <-shellDone:
			if err != nil {
				logger.Printf("Shell error: %v", err)
			}
		}
	}

	tunnels.Close()
	endpoint.Close()

	return nil
}

func runDown(cmd *cobra.Command, args []string) error {
	workspacePath, err := getWorkspacePath(args)
	if err != nil {
		return err
	}

	logger.Printf("Stopping devcontainer at %s...", workspacePath)

	if err := host.DevcontainerDown(workspacePath); err != nil {
		return fmt.Errorf("failed to stop devcontainer: %w", err)
	}

	logger.Printf("Container stopped")
	return nil
}

func runShell(cmd *cobra.Command, args []string) error {
	workspacePath, err := getWorkspacePath(args)
	if err != nil {
		return err
	}

	// Get container ID
	containerID, err := host.GetContainerID(workspacePath)
	if err != nil {
		return fmt.Errorf("failed to get container ID: %w", err)
	}

	return host.ExecShell(containerID, "")
}

func runStatus(cmd *cobra.Command, args []string) error {
	workspacePath, err := getWorkspacePath(args)
	if err != nil {
		return err
	}

	// Get container ID
	containerID, err := host.GetContainerID(workspacePath)
	if err != nil {
		return fmt.Errorf("no running container found for %s", workspacePath)
	}

	if !host.IsContainerRunning(containerID) {
		fmt.Printf("Container %s is not running\n", containerID[:12])
		return nil
	}

	fmt.Printf("Container: %s\n", containerID[:12])
	fmt.Printf("Status: running\n")

	return nil
}
