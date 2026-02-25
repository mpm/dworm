package tui

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"

	"github.com/creack/pty"
	"golang.org/x/term"
)

// Common clear screen sequences that we need to detect
var clearSequences = [][]byte{
	[]byte("\x1b[2J"), // Clear entire screen
	[]byte("\x1b[3J"), // Clear entire screen including scrollback
	[]byte("\x1bc"),   // Full terminal reset
	[]byte("\x1b[H"),  // Cursor home (often used with 2J)
}

const (
	ctrlG = 0x07 // Ctrl+G key code

	// ANSI escape sequences
	csi               = "\x1b["
	saveCursor        = csi + "s"
	restoreCursor     = csi + "u"
	showCursor        = csi + "?25h"
	clearLine         = csi + "2K"
	resetScrollRegion = csi + "r"
)

// Model manages the TUI session
type Model struct {
	containerID   string
	containerName string
	workDir       string
	envVars       map[string]string

	statusBar     *StatusBar
	portUpdateCh  <-chan []PortMapping
	logUpdateCh   <-chan struct{}
	logBuffer     *LogBuffer
	outputMu      sync.Mutex
	output        io.Writer
	ptmx          *os.File
	origTermState *term.State
	doneCh        chan struct{}
	width         int
	height        int
}

// New creates a new TUI model
func New(containerID, containerName, workDir string, envVars map[string]string) *Model {
	return &Model{
		containerID:   containerID,
		containerName: containerName,
		workDir:       workDir,
		envVars:       envVars,
		doneCh:        make(chan struct{}),
	}
}

// Run starts the TUI session and blocks until it exits.
// logBuffer and logUpdateCh may be nil if log routing is not enabled.
func Run(containerID, containerName, workDir string, envVars map[string]string, portUpdateCh <-chan []PortMapping, logBuffer *LogBuffer, logUpdateCh <-chan struct{}) error {
	// Check if we have a TTY
	if !IsTerminal(int(os.Stdin.Fd())) || !IsTerminal(int(os.Stdout.Fd())) {
		return runFallback(containerID, containerName, workDir, envVars)
	}

	m := New(containerID, containerName, workDir, envVars)
	m.portUpdateCh = portUpdateCh
	m.logBuffer = logBuffer
	m.logUpdateCh = logUpdateCh
	return m.run()
}

func (m *Model) run() error {
	// Get terminal size
	width, height, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		return fmt.Errorf("failed to get terminal size: %w", err)
	}
	m.width = width
	m.height = height
	m.output = os.Stdout

	// Create status bar
	m.statusBar = NewStatusBar(m.containerName)
	m.statusBar.SetSize(width, height)

	// Put terminal in raw mode
	m.origTermState, err = term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return fmt.Errorf("failed to set raw mode: %w", err)
	}
	defer m.restore()

	// Build docker exec command
	cmd := m.buildDockerCommand()

	// Calculate shell height (leave room for status bar)
	shellHeight := height - m.statusBar.Height()

	// Start with PTY
	m.ptmx, err = pty.StartWithSize(cmd, &pty.Winsize{
		Rows: uint16(shellHeight),
		Cols: uint16(width),
	})
	if err != nil {
		return fmt.Errorf("failed to start PTY: %w", err)
	}
	defer m.ptmx.Close()

	// Set up scroll region (leave bottom row(s) for status bar)
	m.writeString(setScrollRegion(1, shellHeight))
	defer m.writeString(resetScrollRegion)

	// Clear the entire visible area
	m.clearScreen(shellHeight, height)

	// Load any pre-existing log lines into the status bar
	if m.logBuffer != nil {
		m.statusBar.SetLogs(m.logBuffer.Lines())
	}

	// Initial status bar render
	m.renderStatusBar()

	// Handle signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGWINCH, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	// Start goroutines
	errCh := make(chan error, 3)

	// Output goroutine: PTY -> terminal
	go func() {
		m.copyOutput()
		errCh <- nil
	}()

	// Input goroutine: terminal -> PTY
	go func() {
		m.handleInput()
		errCh <- nil
	}()

	// Signal and port update handler
	go func() {
		m.handleEvents(sigCh)
		errCh <- nil
	}()

	// Wait for shell to exit
	cmdErr := cmd.Wait()

	// Signal done to all goroutines
	close(m.doneCh)

	// Clear status bar before exit
	m.clearStatusBar()

	if cmdErr != nil {
		if exitErr, ok := cmdErr.(*exec.ExitError); ok {
			return fmt.Errorf("exit %d", exitErr.ExitCode())
		}
		return cmdErr
	}

	return nil
}

func (m *Model) buildDockerCommand() *exec.Cmd {
	args := []string{"exec", "-it"}

	if m.workDir != "" {
		args = append(args, "-w", m.workDir)
	}

	for key, value := range m.envVars {
		args = append(args, "-e", key+"="+value)
	}

	args = append(args, m.containerID, "/bin/bash")

	return exec.Command("docker", args...)
}

func (m *Model) copyOutput() {
	buf := make([]byte, 4096)
	for {
		select {
		case <-m.doneCh:
			return
		default:
		}

		n, err := m.ptmx.Read(buf)
		if err != nil {
			return
		}
		if n > 0 {
			data := buf[:n]
			m.write(data)

			// Check if output contains a clear screen sequence
			if m.containsClearSequence(data) {
				m.renderStatusBar()
			}
		}
	}
}

func (m *Model) containsClearSequence(data []byte) bool {
	for _, seq := range clearSequences {
		if bytes.Contains(data, seq) {
			return true
		}
	}
	return false
}

func (m *Model) handleInput() {
	buf := make([]byte, 1)
	for {
		select {
		case <-m.doneCh:
			return
		default:
		}

		n, err := os.Stdin.Read(buf)
		if err != nil || n == 0 {
			continue
		}

		b := buf[0]

		// Check if panel is expanded - any key closes it
		if m.statusBar.IsExpanded() {
			m.clearMaxStatusArea()
			m.statusBar.SetExpanded(false)
			m.resizeAndRender()
			continue
		}

		// Check for Ctrl+G
		if b == ctrlG {
			m.clearMaxStatusArea()
			m.statusBar.ToggleExpanded()
			m.resizeAndRender()
			continue
		}

		// Forward to PTY
		m.ptmx.Write(buf[:n])
	}
}

func (m *Model) handleEvents(sigCh chan os.Signal) {
	for {
		select {
		case <-m.doneCh:
			return

		case sig := <-sigCh:
			switch sig {
			case syscall.SIGWINCH:
				m.handleResize()
			case syscall.SIGINT, syscall.SIGTERM:
				if m.ptmx != nil {
					m.ptmx.Write([]byte{0x03}) // Ctrl+C
				}
			}

		case ports := <-m.portUpdateCh:
			if m.statusBar != nil {
				m.statusBar.SetPorts(ports)
				m.renderStatusBar()
			}

		case <-m.logUpdateCh:
			if m.statusBar != nil && m.logBuffer != nil {
				m.statusBar.SetLogs(m.logBuffer.Lines())
				if m.statusBar.IsExpanded() {
					m.renderStatusBar()
				}
			}
		}
	}
}

func (m *Model) handleResize() {
	// Clear status at old position
	m.clearStatusBar()

	// Get new terminal size
	width, height, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		return
	}
	m.width = width
	m.height = height

	// Update status bar dimensions
	m.statusBar.SetSize(width, height)
	shellHeight := height - m.statusBar.Height()

	// Update scroll region
	m.outputMu.Lock()
	fmt.Fprint(m.output, setScrollRegion(1, shellHeight))
	for row := shellHeight + 1; row <= height; row++ {
		fmt.Fprint(m.output, moveToRow(row))
		fmt.Fprint(m.output, clearLine)
	}
	m.outputMu.Unlock()

	// Resize PTY
	pty.Setsize(m.ptmx, &pty.Winsize{
		Rows: uint16(shellHeight),
		Cols: uint16(m.width),
	})

	// Render status bar
	m.renderStatusBar()
}

func (m *Model) resizeAndRender() {
	width, height, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		return
	}
	m.width = width
	m.height = height

	m.statusBar.SetSize(width, height)
	shellHeight := height - m.statusBar.Height()

	// Update scroll region
	m.outputMu.Lock()
	fmt.Fprint(m.output, setScrollRegion(1, shellHeight))
	for row := shellHeight + 1; row <= height; row++ {
		fmt.Fprint(m.output, moveToRow(row))
		fmt.Fprint(m.output, clearLine)
	}
	m.outputMu.Unlock()

	// Resize PTY
	pty.Setsize(m.ptmx, &pty.Winsize{
		Rows: uint16(shellHeight),
		Cols: uint16(m.width),
	})

	m.renderStatusBar()
}

func (m *Model) renderStatusBar() {
	statusHeight := m.statusBar.Height()
	startRow := m.height - statusHeight + 1

	// Render using lipgloss-styled view
	view := m.statusBar.View()
	lines := bytes.Split([]byte(view), []byte("\n"))

	m.outputMu.Lock()
	defer m.outputMu.Unlock()

	fmt.Fprint(m.output, saveCursor)
	for i, line := range lines {
		row := startRow + i
		if row > m.height {
			break
		}
		fmt.Fprint(m.output, moveToRow(row))
		fmt.Fprint(m.output, clearLine)
		m.output.Write(line)
	}
	fmt.Fprint(m.output, restoreCursor)
}

func (m *Model) clearStatusBar() {
	statusHeight := m.statusBar.Height()
	startRow := m.height - statusHeight + 1

	m.outputMu.Lock()
	defer m.outputMu.Unlock()

	fmt.Fprint(m.output, saveCursor)
	for row := startRow; row <= m.height; row++ {
		fmt.Fprint(m.output, moveToRow(row))
		fmt.Fprint(m.output, clearLine)
	}
	fmt.Fprint(m.output, restoreCursor)
}

func (m *Model) clearMaxStatusArea() {
	// Clear the maximum possible status area
	maxHeight := m.height / 2
	if maxHeight < 5 {
		maxHeight = 5
	}
	startRow := m.height - maxHeight + 1
	if startRow < 1 {
		startRow = 1
	}

	m.outputMu.Lock()
	defer m.outputMu.Unlock()

	fmt.Fprint(m.output, saveCursor)
	for row := startRow; row <= m.height; row++ {
		fmt.Fprint(m.output, moveToRow(row))
		fmt.Fprint(m.output, clearLine)
	}
	fmt.Fprint(m.output, restoreCursor)
}

func (m *Model) clearScreen(shellHeight, totalHeight int) {
	m.outputMu.Lock()
	defer m.outputMu.Unlock()

	// Clear all rows
	for row := 1; row <= totalHeight; row++ {
		fmt.Fprint(m.output, moveToRow(row))
		fmt.Fprint(m.output, clearLine)
	}
	// Move cursor to top-left
	fmt.Fprint(m.output, moveTo(1, 1))
}

func (m *Model) write(data []byte) {
	m.outputMu.Lock()
	defer m.outputMu.Unlock()
	m.output.Write(data)
}

func (m *Model) writeString(s string) {
	m.outputMu.Lock()
	defer m.outputMu.Unlock()
	fmt.Fprint(m.output, s)
}

func (m *Model) restore() {
	if m.origTermState != nil {
		term.Restore(int(os.Stdin.Fd()), m.origTermState)
	}
	m.outputMu.Lock()
	fmt.Fprint(m.output, resetScrollRegion)
	fmt.Fprint(m.output, showCursor)
	m.outputMu.Unlock()
}

// ANSI helper functions
func setScrollRegion(top, bottom int) string {
	return fmt.Sprintf("%s%d;%dr", csi, top, bottom)
}

func moveToRow(row int) string {
	return fmt.Sprintf("%s%d;1H", csi, row)
}

func moveTo(row, col int) string {
	return fmt.Sprintf("%s%d;%dH", csi, row, col)
}
