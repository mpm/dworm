package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
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
	"golang.org/x/term"
)

var (
	envVars       []string
	configPath    string
	bindAddr      string
	execDir       string
	statusJSON    bool
	upDetach      bool
	upForeground  bool
	upDaemon      bool
	verbose       bool
	ensureTimeout time.Duration
	noStart       bool
	noBridge      bool
	logsFollow    bool
	logsJSON      bool
	logsLines     int
	crStderr      = protocol.NewLogWriter(os.Stderr)
	crStdout      = protocol.NewLogWriter(os.Stdout)
	logger        = log.New(crStderr, "", log.LstdFlags)
)

// exitAlreadyRunning is the exit code of `dworm up --foreground` when an
// instance already holds the workspace lock.
const exitAlreadyRunning = 3

// stopTimeout bounds how long stopping an instance may take.
const stopTimeout = 30 * time.Second

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
		Short: "Start the workspace instance (container and bridge) and open a shell",
		Long: `Make sure the workspace instance runs: the long-lived background process that
owns the container's bridge (port forwarding, agent and credential forwarding,
the exec socket). It is started detached if needed.

On a terminal, dworm up then opens the interactive shell; leaving the shell
leaves the instance running (dworm stop or dworm down end it). With -d, or
without a terminal, it waits until the instance is ready and exits.`,
		Args: cobra.NoArgs,
		RunE: runOperationalCommand(runUp),
	}
	upCmd.Flags().BoolVarP(&upDetach, "detach", "d", false, "Only ensure the instance runs; don't open a shell")
	upCmd.Flags().BoolVar(&upForeground, "foreground", false, "Run the instance in the foreground (for systemd)")
	upCmd.Flags().BoolVar(&upDaemon, "daemon", false, "Alias of --foreground")
	upCmd.Flags().MarkDeprecated("daemon", "use --foreground")
	upCmd.Flags().StringVar(&bindAddr, "bind", "127.0.0.1", "Address to bind forwarded ports to (e.g., 0.0.0.0 for all interfaces)")
	addEnsureFlags(upCmd)
	rootCmd.AddCommand(upCmd)

	instanceCmd := &cobra.Command{
		Use:    host.InstanceCommand,
		Short:  "Run a detached instance (started by other dworm commands)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: runOperationalCommand(func(cmd *cobra.Command, args []string) error {
			return runInstance(cmd, host.ModeDetached)
		}),
	}
	instanceCmd.Flags().StringVar(&bindAddr, "bind", "127.0.0.1", "Address to bind forwarded ports to")
	rootCmd.AddCommand(instanceCmd)

	rootCmd.AddCommand(&cobra.Command{
		Use:   "stop",
		Short: "Stop the workspace instance and leave the container running",
		Args:  cobra.NoArgs,
		RunE:  runOperationalCommand(runStop),
	})

	// Down command
	downCmd := &cobra.Command{
		Use:   "down",
		Short: "Stop the workspace instance and the devcontainer",
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
	addEnsureFlags(shellCmd)
	shellCmd.Flags().BoolVar(&noStart, "no-start", false, "Fail instead of starting a stopped container")
	rootCmd.AddCommand(shellCmd)

	// Exec command
	execCmd := &cobra.Command{
		Use:   "exec -- COMMAND [ARG...]",
		Short: "Run a command in the container and exit",
		Args:  cobra.MinimumNArgs(1),
		RunE:  runOperationalCommand(runExec),
	}
	execCmd.Flags().StringVarP(&execDir, "workdir", "w", "", "Working directory inside the container (default: the workspace folder)")
	execCmd.Flags().BoolVar(&noStart, "no-start", false, "Fail instead of starting a stopped container")
	execCmd.Flags().BoolVar(&noBridge, "no-bridge", false, "Run through plain `docker exec` instead of the instance (no forwarding, no process cleanup)")
	addEnsureFlags(execCmd)
	rootCmd.AddCommand(execCmd)

	logsCmd := &cobra.Command{
		Use:   "logs",
		Short: "Show the workspace instance's log and events",
		Long: `Show recent log lines of the workspace instance, then exit. With -f, keep
following its events (starting the instance if needed). --json prints the raw
event stream (state, ports, log, exec_started, exec_exited, client, dropped).`,
		Args: cobra.NoArgs,
		RunE: runOperationalCommand(runLogs),
	}
	logsCmd.Flags().BoolVarP(&logsFollow, "follow", "f", false, "Follow new events (starts the instance if needed)")
	logsCmd.Flags().BoolVar(&logsJSON, "json", false, "Print events as JSON lines")
	logsCmd.Flags().IntVarP(&logsLines, "lines", "n", 100, fmt.Sprintf("Number of recent log lines to show (max %d)", host.EventHistorySize))
	addEnsureFlags(logsCmd)
	rootCmd.AddCommand(logsCmd)

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

func addEnsureFlags(cmd *cobra.Command) {
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "Show instance startup progress even when stderr is not a terminal")
	cmd.Flags().DurationVar(&ensureTimeout, "timeout", host.DefaultEnsureTimeout, "How long to wait for the instance to become ready")
}

// runInstance runs the workspace instance in this process.
func runInstance(cmd *cobra.Command, mode string) error {
	workspacePath, err := getWorkspacePath()
	if err != nil {
		return err
	}
	cfgPath, err := getConfigPath()
	if err != nil {
		return err
	}
	opts := host.InstanceOptions{
		WorkspacePath: workspacePath,
		ConfigPath:    cfgPath,
		Env:           parseEnvVars(),
		Mode:          mode,
		Version:       version.Version,
		Output:        crStderr,
	}
	if cmd.Flags().Changed("bind") {
		opts.BindAddr = bindAddr
	}
	if mode == host.ModeDetached {
		paths := host.InstancePathsFor(workspacePath)
		opts.LogPath = paths.Log
		opts.OpenOutput = func() (io.Writer, error) {
			return host.OpenRotatingLog(paths.Log, host.MaxInstanceLogSize, true)
		}
	}

	// SIGINT/SIGTERM stop the instance cleanly (exit 0, container keeps running).
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	err = host.RunInstance(ctx, opts)
	if errors.Is(err, host.ErrAlreadyRunning) {
		holder := ""
		if state, stateErr := host.ReadInstanceState(host.InstancePathsFor(workspacePath).State); stateErr == nil {
			holder = fmt.Sprintf(" (pid %d)", state.PID)
		}
		return &exitCodeError{code: exitAlreadyRunning, err: fmt.Errorf(
			"a dworm instance is already running for %s%s; use 'dworm shell' or 'dworm exec' with it, or 'dworm stop' it", workspacePath, holder)}
	}
	return err
}

// ensureInstance makes sure the workspace instance runs and is ready. With
// applyEnv, -e values are passed to an instance this call starts.
func ensureInstance(cmd *cobra.Command, applyEnv bool) (*host.InstanceState, error) {
	workspacePath, err := getWorkspacePath()
	if err != nil {
		return nil, err
	}
	var args []string
	if cfgPath, err := getConfigPath(); err != nil {
		return nil, err
	} else if cfgPath != "" {
		args = append(args, "-c", cfgPath)
	}
	var env map[string]string
	if applyEnv {
		env = parseEnvVars()
		if err := config.Validate(env); err != nil {
			return nil, err
		}
		for _, e := range envVars {
			args = append(args, "-e", e)
		}
	}
	if flag := cmd.Flags().Lookup("bind"); flag != nil && flag.Changed {
		args = append(args, "--bind", bindAddr)
	}
	var progress io.Writer
	if verbose || term.IsTerminal(int(os.Stderr.Fd())) {
		progress = crStderr
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return host.Ensure(ctx, host.EnsureOptions{
		WorkspacePath: workspacePath,
		SpawnArgs:     args,
		Env:           env,
		NoStart:       noStart,
		Timeout:       ensureTimeout,
		Progress:      progress,
		Warnings:      crStderr,
		Version:       version.Version,
	})
}

func runUp(cmd *cobra.Command, args []string) error {
	if upForeground || upDaemon {
		return runInstance(cmd, host.ModeForeground)
	}
	state, err := ensureInstance(cmd, true)
	if err != nil {
		return err
	}
	if upDetach || !host.StdioIsTerminal() {
		fmt.Fprintln(os.Stdout, instanceSummary(state))
		return nil
	}
	// The shell's exit status is not dworm up's.
	var exitErr *host.ExitError
	if err := runShellSession(state); err != nil && !errors.As(err, &exitErr) {
		return err
	}
	return nil
}

// instanceSummary is the one-line summary of `dworm up -d`.
func instanceSummary(state *host.InstanceState) string {
	ports := "none"
	if len(state.Ports) > 0 {
		parts := make([]string, len(state.Ports))
		for i, p := range state.Ports {
			parts[i] = fmt.Sprintf("%d", p.Port)
			if p.LocalPort != p.Port {
				parts[i] = fmt.Sprintf("%d->%d", p.LocalPort, p.Port)
			}
		}
		ports = strings.Join(parts, " ")
	}
	return fmt.Sprintf("dworm instance ready: container %s, pid %d, ports: %s", state.ContainerName, state.PID, ports)
}

// runShellSession opens the interactive shell of a ready instance: the TUI
// with status bar on a terminal, a plain framed exec otherwise.
func runShellSession(state *host.InstanceState) error {
	return tui.Run(tui.Config{
		SocketPath:    state.ExecSocket,
		ContainerName: state.ContainerName,
		// The launcher sets up the environment reload hook for Bash.
		Argv: protocol.EnvironmentCommand(map[string]string{}, true, []string{"/bin/bash"}),
	})
}

func runStop(cmd *cobra.Command, args []string) error {
	workspacePath, err := getWorkspacePath()
	if err != nil {
		return err
	}
	stopped, err := host.StopInstance(workspacePath, stopTimeout)
	if err != nil {
		return err
	}
	if stopped {
		logger.Printf("Instance stopped")
	} else {
		logger.Printf("No dworm instance is running for %s", workspacePath)
	}
	return nil
}

// stopInstanceFirst stops a running instance before container operations.
func stopInstanceFirst(workspacePath string) error {
	stopped, err := host.StopInstance(workspacePath, stopTimeout)
	if err != nil {
		return fmt.Errorf("stop the dworm instance: %w", err)
	}
	if stopped {
		logger.Printf("Instance stopped")
	}
	return nil
}

func runDown(cmd *cobra.Command, args []string) error {
	workspacePath, err := getWorkspacePath()
	if err != nil {
		return err
	}
	if err := stopInstanceFirst(workspacePath); err != nil {
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
	state, err := ensureInstance(cmd, true)
	if err != nil {
		return err
	}
	return runShellSession(state)
}

func runExec(cmd *cobra.Command, args []string) error {
	if noBridge {
		return runExecNoBridge(args)
	}
	state, err := ensureInstance(cmd, false)
	if err != nil {
		return err
	}
	// The instance's endpoint terminates the process (group or TTY session)
	// when this client goes away.
	req := protocol.ExecRequest{Argv: args, Cwd: execDir, Env: parseEnvVars()}
	if host.StdioIsTerminal() {
		return host.ExecTTY(state.ExecSocket, req)
	}
	return host.ExecViaSocket(state.ExecSocket, req, os.Stdin, os.Stdout, os.Stderr)
}

// runExecNoBridge runs the command through plain `docker exec`.
func runExecNoBridge(args []string) error {
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
	if err := stopInstanceFirst(workspacePath); err != nil {
		return err
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
	if err := stopInstanceFirst(workspacePath); err != nil {
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

func runLogs(cmd *cobra.Command, args []string) error {
	if logsLines < 0 || logsLines > host.EventHistorySize {
		return fmt.Errorf("--lines must be between 0 and %d", host.EventHistorySize)
	}
	workspacePath, err := getWorkspacePath()
	if err != nil {
		return err
	}
	paths := host.InstancePathsFor(workspacePath)
	socket := paths.Socket
	if logsFollow {
		state, err := ensureInstance(cmd, false)
		if err != nil {
			return err
		}
		socket = state.ExecSocket
	} else if running, err := host.InstanceRunning(paths); err != nil {
		return err
	} else if !running {
		hint := ""
		if _, err := os.Stat(paths.Log); err == nil {
			hint = fmt.Sprintf(" (the last detached instance logged to %s)", paths.Log)
		}
		return fmt.Errorf("no dworm instance is running for %s%s", workspacePath, hint)
	}

	events, err := host.SubscribeEvents(socket, logsLines, logsFollow)
	if err != nil {
		return err
	}
	defer events.Close()
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	for {
		line, err := events.NextRaw()
		if err != nil {
			return nil // the instance ended the stream
		}
		if logsJSON {
			out.Write(append(line, '\n'))
		} else {
			var event host.Event
			if json.Unmarshal(line, &event) != nil {
				continue
			}
			fmt.Fprintln(out, formatEvent(event))
		}
		if logsFollow {
			out.Flush()
		}
	}
}

// formatEvent renders an event for `dworm logs`.
func formatEvent(e host.Event) string {
	ts := e.T.Local().Format("2006-01-02 15:04:05")
	switch e.Type {
	case host.EventLog:
		return fmt.Sprintf("%s [%s] %s", ts, e.Source, e.Message)
	case host.EventState:
		if e.Reason != "" {
			return fmt.Sprintf("%s state: %s (%s)", ts, e.State, e.Reason)
		}
		return fmt.Sprintf("%s state: %s", ts, e.State)
	case host.EventPorts:
		ports := make([]string, len(e.Ports))
		for i, p := range e.Ports {
			ports[i] = fmt.Sprintf("%s:%d->%d", p.Address, p.LocalPort, p.Port)
		}
		if len(ports) == 0 {
			return ts + " ports: none"
		}
		return ts + " ports: " + strings.Join(ports, " ")
	case host.EventExecStarted:
		tty := ""
		if e.TTY {
			tty = " (tty)"
		}
		return fmt.Sprintf("%s exec %s started: %s%s", ts, e.ID, strings.Join(e.Argv, " "), tty)
	case host.EventExecExited:
		switch {
		case e.Error != "":
			return fmt.Sprintf("%s exec %s ended: %s", ts, e.ID, e.Error)
		case e.Signal != "":
			return fmt.Sprintf("%s exec %s exited: signal %s", ts, e.ID, e.Signal)
		default:
			return fmt.Sprintf("%s exec %s exited: code %d", ts, e.ID, e.Code)
		}
	case host.EventClient:
		return fmt.Sprintf("%s clients attached: %d", ts, e.Attached)
	case host.EventDropped:
		return fmt.Sprintf("%s (%d events dropped)", ts, e.Count)
	}
	return fmt.Sprintf("%s %s", ts, e.Type)
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
	if up.ExecSocket != "" {
		fmt.Fprintf(w, "  Exec socket:      %s\n", up.ExecSocket)
	}
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
