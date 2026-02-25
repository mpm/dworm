package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/mpm/dworm/internal/host"
	"github.com/mpm/dworm/internal/host/tui"
	"github.com/mpm/dworm/internal/protocol"
	"github.com/mpm/dworm/internal/version"
	"github.com/spf13/cobra"
)

var (
	envVars    []string
	daemonMode bool
	configPath string
	bindAddr   string
	crStderr   = protocol.NewCRWriter(os.Stderr)
	crStdout   = protocol.NewCRWriter(os.Stdout)
	logger     = log.New(crStderr, "", log.LstdFlags)
)

func main() {
	rootCmd := &cobra.Command{
		Use:     "dworm",
		Short:   "Development Wormhole - seamless devcontainer bridging",
		Version: version.Short(),
		Long: `dworm wraps the devcontainer CLI to provide seamless development
environment bridging between your host machine and containerized development
environments.`,
	}

	// Customize version template to show full info
	rootCmd.SetVersionTemplate(version.Info() + "\n")

	// Global flags
	rootCmd.PersistentFlags().StringArrayVarP(&envVars, "env", "e", nil, "Environment variables to inject (KEY=VALUE)")
	rootCmd.PersistentFlags().StringVarP(&configPath, "config", "c", "", "Path to devcontainer.json or .devcontainer directory")

	// Up command
	upCmd := &cobra.Command{
		Use:   "up",
		Short: "Start container, inject endpoint, establish tunnel",
		Args:  cobra.NoArgs,
		RunE:  runUp,
	}
	upCmd.Flags().BoolVar(&daemonMode, "daemon", false, "Run in daemon mode (no shell)")
	upCmd.Flags().StringVar(&bindAddr, "bind", "127.0.0.1", "Address to bind forwarded ports to (e.g., 0.0.0.0 for all interfaces)")
	rootCmd.AddCommand(upCmd)

	// Down command
	downCmd := &cobra.Command{
		Use:   "down",
		Short: "Stop the devcontainer",
		Args:  cobra.NoArgs,
		RunE:  runDown,
	}
	rootCmd.AddCommand(downCmd)

	// Shell command
	shellCmd := &cobra.Command{
		Use:   "shell",
		Short: "Open a shell in the container",
		Args:  cobra.NoArgs,
		RunE:  runShell,
	}
	rootCmd.AddCommand(shellCmd)

	// Status command
	statusCmd := &cobra.Command{
		Use:   "status",
		Short: "Show forwarded ports and active configuration",
		Args:  cobra.NoArgs,
		RunE:  runStatus,
	}
	rootCmd.AddCommand(statusCmd)

	// Remove command
	var forceRemove bool
	removeCmd := &cobra.Command{
		Use:   "remove",
		Short: "Stop, remove container and its image",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRemove(cmd, args, forceRemove)
		},
	}
	removeCmd.Flags().BoolVarP(&forceRemove, "force", "f", false, "Skip confirmation prompt")
	rootCmd.AddCommand(removeCmd)

	// Rebuild command
	rebuildCmd := &cobra.Command{
		Use:   "rebuild",
		Short: "Rebuild the devcontainer from scratch",
		Args:  cobra.NoArgs,
		RunE:  runRebuild,
	}
	rootCmd.AddCommand(rebuildCmd)

	// Start version check in background (non-blocking)
	updateCh := make(chan *version.CheckResult, 1)
	go func() {
		updateCh <- version.CheckForUpdate()
	}()

	err := rootCmd.Execute()

	// Check for update result (non-blocking)
	select {
	case result := <-updateCh:
		if result != nil && result.UpdateAvailable {
			fmt.Fprintf(os.Stderr, "\nA new version of dworm is available: %s (current: %s)\n", result.Latest, result.Current)
			fmt.Fprintf(os.Stderr, "Download: %s\n", result.ReleaseURL)
		}
	default:
		// Check not complete yet, skip
	}

	if err != nil {
		os.Exit(1)
	}
}

func getWorkspacePath() (string, error) {
	absPath, err := filepath.Abs(".")
	if err != nil {
		return "", fmt.Errorf("failed to get absolute path: %w", err)
	}

	return absPath, nil
}

func getConfigPath() (string, error) {
	if configPath == "" {
		return "", nil
	}

	absPath, err := filepath.Abs(configPath)
	if err != nil {
		return "", fmt.Errorf("failed to get absolute config path: %w", err)
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

// getGPGAgentSocket returns the GPG agent socket path using gpgconf
func getGPGAgentSocket() (string, error) {
	cmd := exec.Command("gpgconf", "--list-dirs", "agent-socket")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("gpgconf failed: %w", err)
	}
	socketPath := strings.TrimSpace(string(output))
	if socketPath == "" {
		return "", fmt.Errorf("gpgconf returned empty socket path")
	}
	// Check if socket exists
	if _, err := os.Stat(socketPath); err != nil {
		return "", fmt.Errorf("GPG agent socket does not exist: %s", socketPath)
	}
	return socketPath, nil
}

func runUp(cmd *cobra.Command, args []string) error {
	workspacePath, err := getWorkspacePath()
	if err != nil {
		return err
	}

	cfgPath, err := getConfigPath()
	if err != nil {
		return err
	}

	logger.Printf("Starting devcontainer at %s...", workspacePath)

	// Start container
	containerInfo, err := host.DevcontainerUp(workspacePath, cfgPath)
	if err != nil {
		return fmt.Errorf("failed to start devcontainer: %w", err)
	}

	logger.Printf("Container started: %s", containerInfo.ContainerName)

	// In interactive (non-daemon) mode, route logs to a buffer for the TUI
	var logBuffer *tui.LogBuffer
	var logUpdateCh <-chan struct{}
	var logWriter io.Writer
	if !daemonMode {
		logBuffer, logUpdateCh = tui.NewLogBuffer(100)
		logWriter = logBuffer
		// Redirect the main logger to the buffer
		logger.SetOutput(logWriter)
	}

	// Find endpoint binary
	endpointPath, err := host.GetEndpointBinaryPath()
	if err != nil {
		return fmt.Errorf("endpoint binary not found: %w", err)
	}

	// Create endpoint manager (pass logWriter; nil in daemon mode falls back to stderr)
	endpoint := host.NewEndpointManager(containerInfo.ContainerID, logWriter)

	// Inject and start endpoint
	if err := endpoint.InjectAndStart(endpointPath); err != nil {
		return fmt.Errorf("failed to start endpoint: %w", err)
	}

	// Check for SSH agent forwarding
	hostSSHSocket := os.Getenv("SSH_AUTH_SOCK")
	sshForward := hostSSHSocket != ""
	containerSSHSocket := protocol.SSHAgentSocketPath

	if !sshForward {
		logger.Printf("Warning: SSH agent forwarding disabled (SSH_AUTH_SOCK not set)")
	} else {
		// Verify socket exists
		if _, err := os.Stat(hostSSHSocket); err != nil {
			logger.Printf("Warning: SSH agent forwarding disabled (socket not found: %s)", hostSSHSocket)
			sshForward = false
		}
	}

	// Check for GPG agent forwarding
	hostGPGSocket, gpgErr := getGPGAgentSocket()
	gpgForward := gpgErr == nil
	containerGPGSocket := protocol.GPGAgentSocketPath

	// Export GPG public keys if forwarding is enabled
	gpgPublicKeys := ""
	if gpgForward {
		exportCmd := exec.Command("gpg", "--export", "--armor")
		if output, err := exportCmd.Output(); err != nil {
			logger.Printf("Warning: failed to export GPG public keys: %v", err)
		} else if len(output) > 0 {
			gpgPublicKeys = string(output)
			logger.Printf("Exported GPG public keys (%d bytes)", len(output))
		}
	}

	if !gpgForward {
		logger.Printf("Warning: GPG agent forwarding disabled (%v)", gpgErr)
	}

	// Read host's git config
	gitConfig := ""
	homeDir, homeErr := os.UserHomeDir()
	if homeErr == nil {
		gitConfigPath := filepath.Join(homeDir, ".gitconfig")
		if content, err := os.ReadFile(gitConfigPath); err == nil {
			gitConfig = string(content)
			logger.Printf("Read host git config (%d bytes)", len(content))
		} else {
			logger.Printf("Warning: could not read host git config: %v", err)
		}
	}

	// Check for git credential forwarding (enabled if git is available on host)
	gitCredForward := false
	if _, err := exec.LookPath("git"); err == nil {
		gitCredForward = true
	} else {
		logger.Printf("Warning: git credential forwarding disabled (git not found on host)")
	}

	// Send init message
	envs := parseEnvVars()
	if err := endpoint.SendInit(envs, sshForward, containerSSHSocket, gpgForward, containerGPGSocket, gitConfig, gitCredForward, gpgPublicKeys); err != nil {
		endpoint.Close()
		return fmt.Errorf("failed to send init: %w", err)
	}

	if sshForward {
		logger.Printf("SSH agent forwarding enabled")
		// Add SSH_AUTH_SOCK to env vars for the shell
		envs["SSH_AUTH_SOCK"] = containerSSHSocket
	}

	if gpgForward {
		logger.Printf("GPG agent forwarding enabled")
		// Note: The endpoint creates the socket at the path GPG expects,
		// so no environment variable is needed - GPG will find it automatically
	}

	if gitCredForward {
		logger.Printf("Git credential forwarding enabled")
	}

	logger.Printf("Endpoint initialized with %d env vars", len(envs))

	// Start agent handler if any forwarding is enabled
	var agentHandler *host.AgentHandler
	if sshForward || gpgForward || gitCredForward {
		agentHandler = host.NewAgentHandler(endpoint.GetMux(), hostSSHSocket, logger)
		if gpgForward {
			agentHandler.SetGPGSocketPath(hostGPGSocket)
		}
		agentHandler.Start()
	}

	// Create tunnel manager and port update channel
	var tunnelLogWriter io.Writer = protocol.NewCRWriter(os.Stderr)
	if logWriter != nil {
		tunnelLogWriter = logWriter
	}
	tunnelLogger := log.New(tunnelLogWriter, "[tunnel] ", log.LstdFlags)
	tunnels := host.NewTunnelManager(endpoint, bindAddr, tunnelLogger)
	portUpdateCh := make(chan []host.PortMapping, 10)
	tunnels.SetPortUpdateChannel(portUpdateCh)

	// Convert port updates to TUI format
	tuiPortUpdateCh := make(chan []tui.PortMapping, 10)
	go func() {
		for ports := range portUpdateCh {
			tuiPorts := make([]tui.PortMapping, len(ports))
			for i, p := range ports {
				tuiPorts[i] = tui.PortMapping{
					ContainerPort: p.ContainerPort,
					LocalPort:     p.LocalPort,
				}
			}
			select {
			case tuiPortUpdateCh <- tuiPorts:
			default:
			}
		}
	}()

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
				tunnels.UpdatePorts(portMsg.Ports)
			}
		}
	}()

	if daemonMode {
		logger.Printf("Running in daemon mode. Press Ctrl+C to stop.")
		<-sigCh
		logger.Printf("Shutting down...")
	} else {
		// Start interactive shell with TUI
		if err := tui.Run(
			containerInfo.ContainerID,
			containerInfo.ContainerName,
			containerInfo.WorkspaceDir,
			envs,
			tuiPortUpdateCh,
			logBuffer,
			logUpdateCh,
		); err != nil {
			// Check if it's just an exit code
			if !strings.HasPrefix(err.Error(), "exit ") {
				logger.Printf("Shell error: %v", err)
			}
		}
	}

	tunnels.Close()
	if agentHandler != nil {
		agentHandler.Close()
	}
	endpoint.Close()

	return nil
}

func runDown(cmd *cobra.Command, args []string) error {
	workspacePath, err := getWorkspacePath()
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
	workspacePath, err := getWorkspacePath()
	if err != nil {
		return err
	}

	// Get container ID
	containerID, err := host.GetContainerID(workspacePath)
	if err != nil {
		return fmt.Errorf("failed to get container ID: %w", err)
	}

	return host.ExecShell(containerID, "", parseEnvVars())
}

func runRemove(cmd *cobra.Command, args []string, force bool) error {
	workspacePath, err := getWorkspacePath()
	if err != nil {
		return err
	}

	if !force {
		fmt.Fprintf(crStdout, "This will remove the devcontainer and its image for %s. Continue? [y/N] ", workspacePath)
		var answer string
		fmt.Scanln(&answer)
		if answer != "y" && answer != "Y" {
			fmt.Fprintln(crStdout, "Aborted.")
			return nil
		}
	}

	logger.Printf("Removing devcontainer at %s...", workspacePath)

	if err := host.DevcontainerRemove(workspacePath, true); err != nil {
		return fmt.Errorf("failed to remove devcontainer: %w", err)
	}

	logger.Printf("Container and image removed")
	return nil
}

func runRebuild(cmd *cobra.Command, args []string) error {
	workspacePath, err := getWorkspacePath()
	if err != nil {
		return err
	}

	cfgPath, err := getConfigPath()
	if err != nil {
		return err
	}

	logger.Printf("Rebuilding devcontainer at %s...", workspacePath)

	containerInfo, err := host.DevcontainerRebuild(workspacePath, cfgPath)
	if err != nil {
		return fmt.Errorf("failed to rebuild devcontainer: %w", err)
	}

	logger.Printf("Container rebuilt: %s", containerInfo.ContainerName)
	return nil
}

func runStatus(cmd *cobra.Command, args []string) error {
	workspacePath, err := getWorkspacePath()
	if err != nil {
		return err
	}

	// Get container ID
	containerID, err := host.GetContainerID(workspacePath)
	if err != nil {
		return fmt.Errorf("no running container found for %s", workspacePath)
	}

	if !host.IsContainerRunning(containerID) {
		fmt.Fprintf(crStdout, "Container %s is not running\n", containerID[:12])
		return nil
	}

	fmt.Fprintf(crStdout, "Container: %s\n", containerID[:12])
	fmt.Fprintf(crStdout, "Status: running\n")

	return nil
}
