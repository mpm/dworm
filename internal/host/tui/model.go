package tui

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/mpm/dworm/internal/host"
	"github.com/mpm/dworm/internal/protocol"
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
	enterAltScreen    = csi + "?1049h"
	exitAltScreen     = csi + "?1049l"
)

// Config describes an interactive shell of a workspace instance.
type Config struct {
	SocketPath    string   // the instance socket
	ContainerName string   // shown in the status bar
	Argv          []string // shell command
}

// Model manages the TUI session
type Model struct {
	cfg Config

	statusBar     *StatusBar
	logBuffer     *LogBuffer
	outputMu      sync.Mutex
	output        io.Writer
	origTermState *term.State
	doneCh        chan struct{}
	width         int
	height        int
	ansiPending   []byte

	sessionMu sync.Mutex
	session   *host.ExecSession // nil while reconnecting
	state     string            // last instance state; guarded by sessionMu
	readyCh   chan struct{}     // signalled when the instance becomes ready
	quitCh    chan struct{}     // closed on SIGTERM/SIGHUP or when the instance stops
	quitOnce  sync.Once
}

// Run opens the shell over the instance socket and blocks until it exits.
// On a terminal it runs on a PTY in the container below a status bar with
// the forwarded ports, instance state, and logs; when the bridge is lost the
// status bar shows the reconnect and a new shell starts once the instance is
// ready again. Without a terminal the shell runs as a plain framed exec. A
// non-zero exit status is returned as *host.ExitError.
func Run(cfg Config) error {
	if !IsTerminal(int(os.Stdin.Fd())) || !IsTerminal(int(os.Stdout.Fd())) {
		return runFallback(cfg)
	}
	m := &Model{
		cfg:     cfg,
		doneCh:  make(chan struct{}),
		readyCh: make(chan struct{}, 1),
		quitCh:  make(chan struct{}),
		state:   host.StateReady,
	}
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

	events, err := host.SubscribeEvents(m.cfg.SocketPath, 100, true)
	if err != nil {
		return fmt.Errorf("subscribe to instance events: %w", err)
	}
	defer events.Close()

	// Create status bar
	m.statusBar = NewStatusBar(m.cfg.ContainerName)
	m.statusBar.SetSize(width, height)
	var logUpdateCh <-chan struct{}
	m.logBuffer, logUpdateCh = NewLogBuffer(100)
	portCh := make(chan []PortMapping, 10)
	stateCh := make(chan string, 10)
	go m.readEvents(events, portCh, stateCh)

	session, err := m.startSession()
	if err != nil {
		return err
	}

	// Put terminal in raw mode
	m.origTermState, err = term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		session.Close()
		return fmt.Errorf("failed to set raw mode: %w", err)
	}
	defer m.restore()
	m.writeString(enterAltScreen)

	// Set up scroll region (leave bottom row(s) for status bar)
	shellHeight := calculateShellHeight(height, m.statusBar.Height())
	m.writeString(setScrollRegion(1, shellHeight))
	defer m.writeString(resetScrollRegion)

	// Clear the entire visible area
	m.clearScreen(shellHeight, height)
	m.renderStatusBar()

	// Handle signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGWINCH, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigCh)

	go m.handleInput()
	go m.handleEvents(sigCh, portCh, stateCh, logUpdateCh)
	defer close(m.doneCh)

	for {
		exit := m.copyOutput(session)
		session.Close()
		m.setSession(nil)
		if exit == nil || exit.Error != protocol.ExecErrorBridgeLost {
			m.clearStatusBar()
			if exit != nil && exit.Code != 0 {
				return &host.ExitError{Code: exit.Code}
			}
			return nil
		}
		m.writeString("\r\n[dworm] Connection to the container lost, reconnecting...\r\n")
		if session, err = m.waitAndRestart(); err != nil {
			m.clearStatusBar()
			return err
		}
	}
}

// startSession starts the shell on a PTY sized to the shell area.
func (m *Model) startSession() (*host.ExecSession, error) {
	termName := os.Getenv("TERM")
	if termName == "" {
		termName = "xterm-256color"
	}
	session, err := host.StartExec(m.cfg.SocketPath, protocol.ExecRequest{
		Argv: m.cfg.Argv,
		TTY:  &protocol.ExecTTY{Rows: calculateShellHeight(m.height, m.statusBar.Height()), Cols: m.width, Term: termName},
	})
	if err != nil {
		return nil, fmt.Errorf("start shell: %w", err)
	}
	m.setSession(session)
	return session, nil
}

// waitAndRestart starts a new shell once the instance is ready again. The
// instance may still report "ready" for a moment after the exec was lost,
// so failed starts are retried.
func (m *Model) waitAndRestart() (*host.ExecSession, error) {
	for {
		if m.currentState() == host.StateReady {
			session, err := m.startSession()
			if err == nil {
				m.writeString("[dworm] Reconnected; this is a new shell.\r\n")
				return session, nil
			}
			if errors.Is(err, host.ErrExecSocketUnavailable) {
				return nil, err
			}
		}
		select {
		case <-m.readyCh:
		case <-m.quitCh:
			return nil, errors.New("the dworm instance stopped")
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (m *Model) setState(state string) {
	m.sessionMu.Lock()
	defer m.sessionMu.Unlock()
	m.state = state
}

func (m *Model) currentState() string {
	m.sessionMu.Lock()
	defer m.sessionMu.Unlock()
	return m.state
}

func (m *Model) setSession(s *host.ExecSession) {
	m.sessionMu.Lock()
	defer m.sessionMu.Unlock()
	m.session = s
}

func (m *Model) currentSession() *host.ExecSession {
	m.sessionMu.Lock()
	defer m.sessionMu.Unlock()
	return m.session
}

func (m *Model) quit() {
	m.quitOnce.Do(func() { close(m.quitCh) })
}

// readEvents feeds instance events to the event handler.
func (m *Model) readEvents(events *host.EventStream, portCh chan<- []PortMapping, stateCh chan<- string) {
	send := func(f func()) bool {
		select {
		case <-m.doneCh:
			return false
		default:
			f()
			return true
		}
	}
	for {
		event, err := events.Next()
		if err != nil {
			send(func() { stateCh <- host.StateStopped })
			return
		}
		ok := true
		switch event.Type {
		case host.EventPorts:
			ports := make([]PortMapping, len(event.Ports))
			for i, p := range event.Ports {
				ports[i] = PortMapping{ContainerPort: p.Port, LocalPort: p.LocalPort}
			}
			ok = send(func() { portCh <- ports })
		case host.EventLog:
			fmt.Fprintf(m.logBuffer, "[%s] %s\n", event.Source, event.Message)
		case host.EventState:
			ok = send(func() { stateCh <- event.State })
		}
		if !ok {
			return
		}
	}
}

// copyOutput writes the session's output until it exits. A broken socket
// connection counts as a lost bridge unless the TUI is quitting.
func (m *Model) copyOutput(session *host.ExecSession) *protocol.ExecExit {
	for {
		frameType, payload, err := session.ReadFrame()
		if err != nil {
			select {
			case <-m.quitCh:
				return nil
			default:
				return &protocol.ExecExit{Code: 255, Error: protocol.ExecErrorBridgeLost}
			}
		}
		switch frameType {
		case protocol.FrameStdout, protocol.FrameStderr:
			if m.writePTYOutput(payload) {
				m.renderStatusBar()
			}
		case protocol.FrameExit:
			return host.ParseExit(payload)
		}
	}
}

func (m *Model) handleInput() {
	buf := make([]byte, 4096)
	for {
		n, err := os.Stdin.Read(buf)
		select {
		case <-m.doneCh:
			return
		default:
		}
		if err != nil {
			return
		}
		input := buf[:n]

		// Check if panel is expanded - any key closes it
		if m.statusBar.IsExpanded() {
			m.clearMaxStatusArea()
			m.statusBar.SetExpanded(false)
			m.resizeAndRender()
			continue
		}

		// Ctrl+G toggles the panel; input after it is dropped.
		toggle := bytes.IndexByte(input, ctrlG)
		if toggle >= 0 {
			input = input[:toggle]
		}
		if session := m.currentSession(); session != nil && len(input) > 0 {
			session.Write(input)
		}
		if toggle >= 0 {
			m.clearMaxStatusArea()
			m.statusBar.ToggleExpanded()
			m.resizeAndRender()
		}
	}
}

func (m *Model) handleEvents(sigCh chan os.Signal, portCh <-chan []PortMapping, stateCh <-chan string, logUpdateCh <-chan struct{}) {
	for {
		select {
		case <-m.doneCh:
			return

		case sig := <-sigCh:
			switch sig {
			case syscall.SIGWINCH:
				m.handleResize()
			case syscall.SIGINT:
				if session := m.currentSession(); session != nil {
					session.Write([]byte{0x03}) // Ctrl+C
				}
			case syscall.SIGTERM, syscall.SIGHUP:
				// Leave: closing the session hangs up the shell.
				m.quit()
				if session := m.currentSession(); session != nil {
					session.Close()
				}
			}

		case ports := <-portCh:
			m.statusBar.SetPorts(ports)
			m.renderStatusBar()

		case state := <-stateCh:
			m.setState(state)
			m.statusBar.SetState(state)
			m.renderStatusBar()
			switch state {
			case host.StateReady:
				select {
				case m.readyCh <- struct{}{}:
				default:
				}
			case host.StateStopping, host.StateStopped, host.StateFailed:
				m.quit()
			}

		case <-logUpdateCh:
			m.statusBar.SetLogs(m.logBuffer.Lines())
			if m.statusBar.IsExpanded() {
				m.renderStatusBar()
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
	shellHeight := calculateShellHeight(height, m.statusBar.Height())
	m.updateTerminalLayout(shellHeight, height)

	m.resizeSession(shellHeight)

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
	shellHeight := calculateShellHeight(height, m.statusBar.Height())
	m.updateTerminalLayout(shellHeight, height)

	m.resizeSession(shellHeight)

	m.renderStatusBar()
}

// resizeSession resizes the shell's PTY in the container.
func (m *Model) resizeSession(shellHeight int) {
	if session := m.currentSession(); session != nil {
		session.Resize(shellHeight, m.width)
	}
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

func (m *Model) updateTerminalLayout(shellHeight, totalHeight int) {
	m.outputMu.Lock()
	defer m.outputMu.Unlock()

	fmt.Fprint(m.output, setScrollRegion(1, shellHeight))
	for row := shellHeight + 1; row <= totalHeight; row++ {
		fmt.Fprint(m.output, moveToRow(row))
		fmt.Fprint(m.output, clearLine)
	}
	// The old cursor may be below a newly shortened region. Always finish inside
	// the shell area so subsequent newlines can scroll instead of one-line looping.
	fmt.Fprint(m.output, moveToRow(shellHeight))
}

func calculateShellHeight(totalHeight, statusHeight int) int {
	shellHeight := totalHeight - statusHeight
	if shellHeight < 1 {
		return 1
	}
	return shellHeight
}

// writePTYOutput keeps incomplete ANSI sequences together and repairs terminal
// state that child applications assume applies to their smaller PTY viewport.
func (m *Model) writePTYOutput(data []byte) bool {
	m.outputMu.Lock()
	defer m.outputMu.Unlock()

	data = append(m.ansiPending, data...)
	m.ansiPending = nil

	complete, pending := splitCompleteANSI(data)
	if len(pending) > 4096 {
		complete = append(complete, pending)
	} else {
		m.ansiPending = append(m.ansiPending, pending...)
	}

	redraw := false
	for _, part := range complete {
		if bytes.Equal(part, []byte(resetScrollRegion)) {
			fmt.Fprint(m.output, setScrollRegion(1, calculateShellHeight(m.height, m.statusBar.Height())))
			redraw = true
			continue
		}

		m.output.Write(part)
		if repairsTerminalState(part) {
			fmt.Fprint(m.output, setScrollRegion(1, calculateShellHeight(m.height, m.statusBar.Height())))
			redraw = true
		}
		if isClearSequence(part) {
			redraw = true
		}
	}
	return redraw
}

func repairsTerminalState(part []byte) bool {
	if bytes.Equal(part, []byte("\x1bc")) {
		return true
	}
	return bytes.Equal(part, []byte(enterAltScreen)) ||
		bytes.Equal(part, []byte(exitAltScreen)) ||
		bytes.Equal(part, []byte("\x1b[?47h")) ||
		bytes.Equal(part, []byte("\x1b[?47l")) ||
		bytes.Equal(part, []byte("\x1b[?1047h")) ||
		bytes.Equal(part, []byte("\x1b[?1047l"))
}

func isClearSequence(part []byte) bool {
	for _, seq := range clearSequences {
		if bytes.Equal(part, seq) {
			return true
		}
	}
	return false
}

func splitCompleteANSI(data []byte) ([][]byte, []byte) {
	parts := make([][]byte, 0, 4)
	plainStart := 0
	for i := 0; i < len(data); {
		if data[i] != '\x1b' {
			i++
			continue
		}
		if i > plainStart {
			parts = append(parts, data[plainStart:i])
		}

		end := ansiSequenceEnd(data, i)
		if end == 0 {
			return parts, data[i:]
		}
		parts = append(parts, data[i:end])
		i = end
		plainStart = i
	}
	if plainStart < len(data) {
		parts = append(parts, data[plainStart:])
	}
	return parts, nil
}

func ansiSequenceEnd(data []byte, start int) int {
	if start+1 >= len(data) {
		return 0
	}

	switch data[start+1] {
	case '[':
		for i := start + 2; i < len(data); i++ {
			if data[i] >= 0x40 && data[i] <= 0x7e {
				return i + 1
			}
		}
		return 0
	case ']', 'P', '^', '_':
		for i := start + 2; i < len(data); i++ {
			if data[i] == '\a' {
				return i + 1
			}
			if data[i] == '\x1b' && i+1 < len(data) && data[i+1] == '\\' {
				return i + 2
			}
		}
		return 0
	default:
		for i := start + 1; i < len(data); i++ {
			if data[i] >= 0x30 && data[i] <= 0x7e {
				return i + 1
			}
		}
		return 0
	}
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
	m.outputMu.Lock()
	fmt.Fprint(m.output, resetScrollRegion)
	fmt.Fprint(m.output, exitAltScreen)
	fmt.Fprint(m.output, showCursor)
	m.outputMu.Unlock()
	if m.origTermState != nil {
		term.Restore(int(os.Stdin.Fd()), m.origTermState)
	}
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
