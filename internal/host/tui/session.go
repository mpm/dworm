package tui

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"golang.org/x/term"
)

const (
	ctrlG = 0x07 // Ctrl+G key code
)

// ShellSession manages an interactive shell with a status line
type ShellSession struct {
	containerID   string
	containerName string
	workDir       string
	envVars       map[string]string

	statusLine   *StatusLine
	portUpdateCh <-chan []PortMapping

	termWriter    *TermWriter
	ptmx          *os.File
	origTermState *term.State
	doneCh        chan struct{}
}

// NewShellSession creates a new shell session
func NewShellSession(containerID, containerName, workDir string, envVars map[string]string, portUpdateCh <-chan []PortMapping) *ShellSession {
	return &ShellSession{
		containerID:   containerID,
		containerName: containerName,
		workDir:       workDir,
		envVars:       envVars,
		portUpdateCh:  portUpdateCh,
		doneCh:        make(chan struct{}),
	}
}

// Run starts the shell session with the TUI
func (s *ShellSession) Run() error {
	// Check if we have a TTY
	if !IsTerminal(int(os.Stdin.Fd())) || !IsTerminal(int(os.Stdout.Fd())) {
		// Fall back to non-TUI mode
		return s.runWithoutTUI()
	}

	// Get terminal size
	width, height, err := GetTerminalSize(int(os.Stdout.Fd()))
	if err != nil {
		return fmt.Errorf("failed to get terminal size: %w", err)
	}

	// Create synchronized terminal writer for all output
	s.termWriter = NewTermWriter(os.Stdout)

	// Create status line with shared writer
	s.statusLine = NewStatusLine(s.termWriter, s.containerName, width, height)

	// Put terminal in raw mode
	s.origTermState, err = MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return fmt.Errorf("failed to set raw mode: %w", err)
	}
	defer s.restore()

	// Build docker exec command
	cmd := s.buildDockerCommand()

	// Calculate shell height (leave room for status line)
	shellHeight := height - s.statusLine.Height()

	// Start with PTY
	s.ptmx, err = StartWithPTY(cmd, shellHeight, width)
	if err != nil {
		return fmt.Errorf("failed to start PTY: %w", err)
	}
	defer s.ptmx.Close()

	// Set up scroll region (leave bottom row(s) for status line)
	s.termWriter.WriteString(SetScrollRegion(1, shellHeight))
	defer s.termWriter.WriteString(ResetScrollRegion)

	// Move cursor to top-left of scroll region
	s.termWriter.WriteString(MoveTo(1, 1))

	// Initial status line render
	s.statusLine.Render()

	// Handle signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGWINCH, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	// Start goroutines
	errCh := make(chan error, 3)

	// Output goroutine: PTY -> terminal
	go func() {
		s.copyOutput()
		errCh <- nil
	}()

	// Input goroutine: terminal -> PTY (with Ctrl+G filtering)
	go func() {
		s.handleInput()
		errCh <- nil
	}()

	// Signal and port update handler
	go func() {
		s.handleEvents(sigCh)
		errCh <- nil
	}()

	// Wait for shell to exit
	cmdErr := cmd.Wait()

	// Signal done to all goroutines
	close(s.doneCh)

	// Clear status line before exit
	s.statusLine.Clear()

	if cmdErr != nil {
		if exitErr, ok := cmdErr.(*exec.ExitError); ok {
			return fmt.Errorf("exit %d", exitErr.ExitCode())
		}
		return cmdErr
	}

	return nil
}

func (s *ShellSession) buildDockerCommand() *exec.Cmd {
	args := []string{"exec", "-it"}

	if s.workDir != "" {
		args = append(args, "-w", s.workDir)
	}

	for key, value := range s.envVars {
		args = append(args, "-e", key+"="+value)
	}

	args = append(args, s.containerID, "/bin/bash")

	return exec.Command("docker", args...)
}

func (s *ShellSession) copyOutput() {
	buf := make([]byte, 4096)
	for {
		select {
		case <-s.doneCh:
			return
		default:
		}

		n, err := s.ptmx.Read(buf)
		if err != nil {
			return
		}
		if n > 0 {
			// Use synchronized writer to prevent interleaving with status line
			s.termWriter.Write(buf[:n])
		}
	}
}

func (s *ShellSession) handleInput() {
	buf := make([]byte, 1)
	for {
		select {
		case <-s.doneCh:
			return
		default:
		}

		n, err := os.Stdin.Read(buf)
		if err != nil {
			return
		}
		if n == 0 {
			continue
		}

		b := buf[0]

		// Check if panel is expanded - any key closes it
		if s.statusLine.IsExpanded() {
			s.statusLine.SetExpanded(false)
			s.resizeAndRender()
			continue
		}

		// Check for Ctrl+G
		if b == ctrlG {
			s.statusLine.ToggleExpanded()
			s.resizeAndRender()
			continue
		}

		// Forward to PTY
		s.ptmx.Write(buf[:n])
	}
}

func (s *ShellSession) handleEvents(sigCh chan os.Signal) {
	for {
		select {
		case <-s.doneCh:
			return

		case sig := <-sigCh:
			switch sig {
			case syscall.SIGWINCH:
				s.handleResize()
			case syscall.SIGINT, syscall.SIGTERM:
				// Forward to shell process via PTY
				// The PTY will handle sending the signal to the child
				if s.ptmx != nil {
					s.ptmx.Write([]byte{0x03}) // Ctrl+C
				}
			}

		case ports := <-s.portUpdateCh:
			if s.statusLine != nil {
				s.statusLine.UpdatePorts(ports)
				s.statusLine.Render()
			}
		}
	}
}

func (s *ShellSession) handleResize() {
	width, height, err := GetTerminalSize(int(os.Stdout.Fd()))
	if err != nil {
		return
	}

	s.statusLine.SetTerminalSize(width, height)
	shellHeight := height - s.statusLine.Height()

	// Update scroll region (use synchronized writer)
	s.termWriter.WriteString(SetScrollRegion(1, shellHeight))

	// Resize PTY
	ResizePTY(s.ptmx, shellHeight, width)

	// Re-render status line
	s.statusLine.Render()
}

func (s *ShellSession) resizeAndRender() {
	width, height, err := GetTerminalSize(int(os.Stdout.Fd()))
	if err != nil {
		return
	}

	s.statusLine.SetTerminalSize(width, height)
	shellHeight := height - s.statusLine.Height()

	// Update scroll region and clear old status area atomically
	s.termWriter.Atomic(func(w io.Writer) {
		fmt.Fprint(w, SetScrollRegion(1, shellHeight))

		// Clear all potential status rows
		for row := shellHeight + 1; row <= height; row++ {
			clearRowTo(w, row, "")
		}
	})

	// Resize PTY
	ResizePTY(s.ptmx, shellHeight, width)

	// Re-render status line
	s.statusLine.Render()
}

func (s *ShellSession) restore() {
	if s.origTermState != nil {
		Restore(int(os.Stdin.Fd()), s.origTermState)
	}
	// Reset scroll region and show cursor
	if s.termWriter != nil {
		s.termWriter.Atomic(func(w io.Writer) {
			fmt.Fprint(w, ResetScrollRegion)
			fmt.Fprint(w, ShowCursor)
		})
	} else {
		fmt.Fprint(os.Stdout, ResetScrollRegion)
		fmt.Fprint(os.Stdout, ShowCursor)
	}
}

// runWithoutTUI runs the shell without the TUI (fallback for non-TTY)
func (s *ShellSession) runWithoutTUI() error {
	cmd := s.buildDockerCommand()
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
		if exitErr, ok := err.(*exec.ExitError); ok {
			return fmt.Errorf("exit %d", exitErr.ExitCode())
		}
		return err
	}

	return nil
}

// Close cleans up the session
func (s *ShellSession) Close() {
	if s.ptmx != nil {
		s.ptmx.Close()
	}
	s.restore()
}
