package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mpm/dworm/internal/config"
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
	execDir    string
	statusJSON bool
	crStderr   = protocol.NewLogWriter(os.Stderr)
	crStdout   = protocol.NewLogWriter(os.Stdout)
	logger     = log.New(crStderr, "", log.LstdFlags)
)

// exitAlreadyRunning is the exit code of `dworm up` when another `dworm up`
// already holds the workspace lock.
const exitAlreadyRunning = 3

// exitCodeError is an error that main prints before exiting with code.
type exitCodeError struct {
	code int
	err  error
}

func (e *exitCodeError) Error() string { return e.err.Error() }
func (e *exitCodeError) Unwrap() error { return e.err }

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
	// Errors are printed in main so commands can exit with their own codes.
	rootCmd.SilenceErrors = true

	// Global flags
	rootCmd.PersistentFlags().StringArrayVarP(&envVars, "env", "e", nil, "Environment variables to inject (KEY=VALUE)")
	rootCmd.PersistentFlags().StringVarP(&configPath, "config", "c", "", "Path to devcontainer.json or .devcontainer directory")

	// Up command
	upCmd := &cobra.Command{
		Use:   "up",
		Short: "Start container, inject endpoint, establish tunnel",
		Args:  cobra.NoArgs,
		RunE:  runOperationalCommand(runUp),
	}
	upCmd.Flags().BoolVar(&daemonMode, "daemon", false, "Run in daemon mode (no shell)")
	upCmd.Flags().StringVar(&bindAddr, "bind", "127.0.0.1", "Address to bind forwarded ports to (e.g., 0.0.0.0 for all interfaces)")
	rootCmd.AddCommand(upCmd)

	// Down command
	downCmd := &cobra.Command{
		Use:   "down",
		Short: "Stop the devcontainer",
		Args:  cobra.NoArgs,
		RunE:  runOperationalCommand(runDown),
	}
	rootCmd.AddCommand(downCmd)

	// Shell command
	shellCmd := &cobra.Command{
		Use:   "shell",
		Short: "Open a shell in the container",
		Args:  cobra.NoArgs,
		RunE:  runOperationalCommand(runShell),
	}
	rootCmd.AddCommand(shellCmd)

	// Exec command
	execCmd := &cobra.Command{
		Use:   "exec -- COMMAND [ARG...]",
		Short: "Run a command in the container and exit",
		Args:  cobra.MinimumNArgs(1),
		RunE:  runOperationalCommand(runExec),
	}
	execCmd.Flags().StringVarP(&execDir, "workdir", "w", "", "Working directory inside the container (default: the workspace folder)")
	rootCmd.AddCommand(execCmd)

	// Status command
	statusCmd := &cobra.Command{
		Use:   "status",
		Short: "Show container, dworm up, and forwarded port status",
		Args:  cobra.NoArgs,
		RunE:  runOperationalCommand(runStatus),
	}
	statusCmd.Flags().BoolVar(&statusJSON, "json", false, "Print machine-readable JSON")
	rootCmd.AddCommand(statusCmd)

	// Remove command
	var forceRemove bool
	removeCmd := &cobra.Command{
		Use:   "remove",
		Short: "Stop, remove container and its image",
		Args:  cobra.NoArgs,
		RunE: runOperationalCommand(func(cmd *cobra.Command, args []string) error {
			return runRemove(cmd, args, forceRemove)
		}),
	}
	removeCmd.Flags().BoolVarP(&forceRemove, "force", "f", false, "Skip confirmation prompt")
	rootCmd.AddCommand(removeCmd)

	// Rebuild command
	rebuildCmd := &cobra.Command{
		Use:   "rebuild",
		Short: "Rebuild the devcontainer from scratch",
		Args:  cobra.NoArgs,
		RunE:  runOperationalCommand(runRebuild),
	}
	rootCmd.AddCommand(rebuildCmd)
	rootCmd.AddCommand(&cobra.Command{
		Use: "self-update", Short: "Install the latest stable dworm release", Args: cobra.NoArgs,
		RunE: runOperationalCommand(func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer cancel()
			return version.SelfUpdate(ctx, cmd.OutOrStdout())
		}),
	})

	// Start version check in background (non-blocking)
	updateCh := make(chan *version.CheckResult, 1)
	selected, _, _ := rootCmd.Find(os.Args[1:])
	if selected == nil || selected.Name() != "self-update" {
		go func() { updateCh <- version.CheckForUpdate() }()
	}

	err := rootCmd.Execute()
	exitCode := 0
	if err != nil {
		exitCode = 1
		var exitErr *host.ExitError
		var codeErr *exitCodeError
		if errors.As(err, &exitErr) {
			// The command's own exit status; it already reported any error.
			exitCode = exitErr.Code
		} else {
			if errors.As(err, &codeErr) {
				exitCode = codeErr.code
			}
			fmt.Fprintln(os.Stderr, "Error:", err)
		}
	}

	// Check for update result (non-blocking)
	select {
	case result := <-updateCh:
		if result != nil && result.UpdateAvailable {
			fmt.Fprintf(os.Stderr, "\nA new version of dworm is available: %s (current: %s)\n", result.Latest, result.Current)
			fmt.Fprintf(os.Stderr, "Run dworm self-update or download: %s\n", result.ReleaseURL)
		}
	default:
		// Check not complete yet, skip
	}

	os.Exit(exitCode)
}

func runOperationalCommand(runE func(*cobra.Command, []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		// Cobra has already validated flags and arguments before RunE starts. Any
		// error from here is operational, so usage instructions would be misleading.
		cmd.Root().SilenceUsage = true
		return runE(cmd, args)
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

	// One `dworm up` per workspace. The kernel drops the lock on exit or crash.
	lock, err := host.AcquireInstanceLock(workspacePath)
	if errors.Is(err, host.ErrAlreadyRunning) {
		holder := ""
		if state, stateErr := host.ReadInstanceState(host.InstancePathsFor(workspacePath).State); stateErr == nil {
			holder = fmt.Sprintf(" (pid %d)", state.PID)
		}
		return &exitCodeError{code: exitAlreadyRunning, err: fmt.Errorf(
			"dworm up is already running for %s%s; use 'dworm shell' or 'dworm exec' alongside it", workspacePath, holder)}
	}
	if err != nil {
		return err
	}
	defer lock.Release()
	state, err := host.NewStateFile(lock.Paths.State, host.InstanceState{
		PID:           os.Getpid(),
		WorkspacePath: workspacePath,
		StartedAt:     time.Now().UTC(),
	})
	if err != nil {
		return fmt.Errorf("write state file: %w", err)
	}
	updateState := func(change func(*host.InstanceState)) {
		if err := state.Update(change); err != nil {
			logger.Printf("Warning: failed to update state file: %v", err)
		}
	}

	projectConfig, staticEnv, err := config.Load(workspacePath)
	if err != nil {
		return err
	}
	if !cmd.Flags().Changed("bind") && projectConfig.Bind != "" {
		bindAddr = projectConfig.Bind
	}
	// stopCtx ends only on SIGINT/SIGTERM: a requested stop exits 0 and leaves
	// the container running. ctx is additionally cancelled on bridge failure.
	stopCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithCancel(stopCtx)
	defer cancel()
	cliEnv := parseEnvVars()
	if err := config.Validate(cliEnv); err != nil {
		return err
	}
	generatedEnv, err := host.RunEnvironmentCommand(ctx, workspacePath, projectConfig.HostEnv, os.Stderr)
	if stopCtx.Err() != nil {
		return nil
	}
	if err != nil {
		return err
	}
	envs := config.Merge(staticEnv, generatedEnv, cliEnv)

	logger.Printf("Starting devcontainer at %s...", workspacePath)

	// Start container
	containerInfo, err := host.DevcontainerUp(workspacePath, cfgPath)
	if err != nil {
		return fmt.Errorf("failed to start devcontainer: %w", err)
	}

	logger.Printf("Container started: %s", containerInfo.ContainerName)
	if stopCtx.Err() != nil {
		return nil
	}
	updateState(func(s *host.InstanceState) {
		s.ContainerID = containerInfo.ContainerID
		s.ContainerName = containerInfo.ContainerName
		s.RemoteUser = containerInfo.RemoteUser
		s.WorkspaceFolder = containerInfo.WorkspaceDir
	})

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

	// Create endpoint manager (pass logWriter; nil in daemon mode falls back to stderr)
	endpoint := host.NewEndpointManager(containerInfo.ContainerID, logWriter)

	// Inject and start endpoint
	if err := endpoint.InjectAndStart(); err != nil {
		if stopCtx.Err() != nil {
			return nil
		}
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
	if err := endpoint.SendInit(envs, sshForward, containerSSHSocket, gpgForward, containerGPGSocket, gitConfig, gitCredForward, gpgPublicKeys); err != nil {
		endpoint.Close()
		return fmt.Errorf("failed to send init: %w", err)
	}
	defer endpoint.Close()
	if err := endpoint.WaitEnvironmentReady(); err != nil {
		if stopCtx.Err() != nil {
			return nil
		}
		return fmt.Errorf("initialize environment: %w", err)
	}
	updateState(func(s *host.InstanceState) { s.EndpointConnected = true })

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
	interval, _, _ := projectConfig.HostEnv.Durations()
	refreshDone := make(chan struct{})
	go func() {
		defer close(refreshDone)
		host.RefreshEnvironment(ctx, interval, func() error {
			generated, err := host.RunEnvironmentCommand(ctx, workspacePath, projectConfig.HostEnv, logger.Writer())
			if err != nil {
				return err
			}
			next := config.Merge(staticEnv, generated, cliEnv)
			if sshForward {
				next["SSH_AUTH_SOCK"] = containerSSHSocket
			}
			return endpoint.GetMux().SendControl(protocol.TypeEnvironment, next)
		}, func(err error) { logger.Printf("Environment refresh failed (keeping previous values): %v", err) })
	}()
	defer func() {
		cancel()
		endpoint.GetMux().Close()
		select {
		case <-refreshDone:
		case <-time.After(2 * time.Second):
		}
	}()

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
	tunnelLogWriter := protocol.NewLogWriter(os.Stderr)
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
			statePorts := make([]host.StatePort, len(ports))
			for i, p := range ports {
				tuiPorts[i] = tui.PortMapping{
					ContainerPort: p.ContainerPort,
					LocalPort:     p.LocalPort,
				}
				statePorts[i] = host.StatePort{Port: p.ContainerPort, Address: bindAddr, LocalPort: p.LocalPort}
			}
			updateState(func(s *host.InstanceState) { s.Ports = statePorts })
			select {
			case tuiPortUpdateCh <- tuiPorts:
			default:
			}
		}
	}()

	// Handle control messages
	transportFailureCh := make(chan error, 1)
	go func() {
		for {
			msgType, data, err := endpoint.RecvControl()
			if err != nil {
				if ctx.Err() != nil {
					// Shutdown closed the bridge.
					return
				}
				transportErr := fmt.Errorf("endpoint transport failed: %w", err)
				cancel()
				updateState(func(s *host.InstanceState) { s.EndpointConnected = false })
				logger.Printf("Forwarding stopped: %v", transportErr)
				tunnels.Close()
				transportFailureCh <- transportErr
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
		select {
		case <-stopCtx.Done():
		case err = <-transportFailureCh:
		}
		// systemd signals the whole cgroup, so the docker exec carrying the
		// bridge may die just before our own SIGTERM is delivered. A stop that
		// arrives with the failure is still a clean shutdown.
		if err != nil && stopRequested(stopCtx, 500*time.Millisecond) {
			err = nil
		}
		if err == nil {
			logger.Printf("Shutting down...")
		}
	} else {
		// Start interactive shell with TUI
		shellErr := tui.Run(
			containerInfo.ContainerID,
			containerInfo.ContainerName,
			containerInfo.WorkspaceDir,
			nil,
			tuiPortUpdateCh,
			logBuffer,
			logUpdateCh,
			transportFailureCh,
		)
		if shellErr != nil {
			// Check if it's just an exit code
			if !strings.HasPrefix(shellErr.Error(), "exit ") {
				logger.Printf("Shell error: %v", shellErr)
				if strings.HasPrefix(shellErr.Error(), "endpoint transport failed:") && !stopRequested(stopCtx, 500*time.Millisecond) {
					err = shellErr
				}
			}
		}
	}

	cancel()
	tunnels.Close()
	if agentHandler != nil {
		agentHandler.Close()
	}
	endpoint.Close()

	return err
}

// stopRequested reports whether SIGINT/SIGTERM arrived, waiting up to grace.
func stopRequested(stopCtx context.Context, grace time.Duration) bool {
	select {
	case <-stopCtx.Done():
		return true
	case <-time.After(grace):
		return false
	}
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

	return host.ExecShell(containerID, host.WorkspaceFolder(containerID, workspacePath), parseEnvVars())
}

func runExec(cmd *cobra.Command, args []string) error {
	workspacePath, err := getWorkspacePath()
	if err != nil {
		return err
	}

	containerID, err := host.GetContainerID(workspacePath)
	if err != nil {
		return fmt.Errorf("failed to get container ID: %w", err)
	}

	workDir := execDir
	if workDir == "" {
		workDir = host.WorkspaceFolder(containerID, workspacePath)
	}
	return host.ExecCommand(containerID, workDir, parseEnvVars(), args)
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

	status, err := host.GetStatus(workspacePath, version.Version)
	if err != nil {
		return err
	}

	if statusJSON {
		encoder := json.NewEncoder(cmd.OutOrStdout())
		encoder.SetIndent("", "  ")
		return encoder.Encode(status)
	}
	printStatus(crStdout, status)
	return nil
}

func printStatus(w io.Writer, status *host.Status) {
	fmt.Fprintf(w, "Workspace:  %s\n", status.WorkspacePath)
	if c := status.Container; c == nil {
		fmt.Fprintf(w, "Container:  none\n")
	} else {
		state := "stopped"
		if c.Running {
			state = "running"
		}
		fmt.Fprintf(w, "Container:  %s (%s), %s\n", c.Name, c.ID, state)
		fmt.Fprintf(w, "  Remote user:      %s\n", valueOrDash(c.RemoteUser))
		fmt.Fprintf(w, "  Workspace folder: %s\n", valueOrDash(c.WorkspaceFolder))
	}

	up := status.Up
	if !up.Running {
		fmt.Fprintf(w, "dworm up:   not running\n")
		return
	}
	since := ""
	if up.StartedAt != nil {
		since = ", since " + up.StartedAt.Local().Format(time.DateTime)
	}
	connection := "endpoint not connected"
	if up.EndpointConnected {
		connection = "endpoint connected"
	}
	fmt.Fprintf(w, "dworm up:   running (pid %d%s), %s\n", up.PID, since, connection)
	if len(up.Ports) == 0 {
		fmt.Fprintf(w, "  Forwarded ports:  none\n")
	}
	for i, p := range up.Ports {
		label := ""
		if i == 0 {
			label = "Forwarded ports:"
		}
		fmt.Fprintf(w, "  %-17s %s:%d -> container:%d\n", label, p.Address, p.LocalPort, p.Port)
	}
}

func valueOrDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}
