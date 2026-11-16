package tui

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mpm/dworm/internal/host"
	"github.com/mpm/dworm/internal/protocol"
	"golang.org/x/term"
)

// Keep a lost session distinct from a command that deliberately exits 255.
var errShellLost = fmt.Errorf("shell lost: %w", &host.ExitError{Code: host.ExitLost})

func shellStopResult(code int) error {
	if code == host.ExitLost {
		return errShellLost
	}
	return &host.ExitError{Code: code}
}

const (
	ctrlG = 0x07 // Ctrl+G key code

	maxOutputBatch = 1 << 20 // child output written to the terminal at once

	// ANSI escape sequences
	csi               = "\x1b["
	saveCursor        = csi + "s"
	restoreCursor     = csi + "u"
	showCursor        = csi + "?25h"
	clearLine         = csi + "2K"
	resetScrollRegion = csi + "r"
	enterAltScreen    = csi + "?1049h"
	exitAltScreen     = csi + "?1049l"
	eraseScreen       = csi + "2J"
	sgrReset          = csi + "m"
	cursorHome        = csi + "H"
	fullReset         = "\x1bc" // RIS
	// softReset stands in for a child's RIS: DECSTR, default attributes, autowrap.
	softReset = csi + "!p" + csi + "m" + csi + "?7h"
)

// Config describes an interactive shell of a workspace instance.
type Config struct {
	SocketPath       string        // the instance socket
	ContainerName    string        // shown in the status bar
	Argv             []string      // shell command
	ReconnectTimeout time.Duration // default: two minutes after a lost shell
}

// Model manages the TUI session
type Model struct {
	cfg Config

	statusBar     *StatusBar
	logBuffer     *LogBuffer
	outputMu      sync.Mutex
	uiMu          sync.Mutex // dimensions and status bar shared by input/events/output
	output        io.Writer
	origTermState *term.State
	doneCh        chan struct{}
	width         int
	height        int
	ansiPending   []byte
	shadow        *shadowScreen // the child's view of the shell area; guarded by outputMu

	sessionMu sync.Mutex
	session   *host.ExecSession // nil while reconnecting
	state     string            // last instance state; guarded by sessionMu
	readyCh   chan struct{}     // signalled when the instance becomes ready
	quitCh    chan struct{}     // closed on SIGTERM/SIGHUP or when the instance stops
	quitOnce  sync.Once
	quitCode  int // guarded by sessionMu; 255 for instance loss
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
	err := m.run()
	var rejected *host.ExecRejectedError
	if errors.As(err, &rejected) && rejected.ExitCode != 0 {
		fmt.Fprintf(os.Stderr, "dworm: %s\n", rejected.Message)
		return &host.ExitError{Code: rejected.ExitCode}
	}
	if errors.Is(err, errShellLost) {
		fmt.Fprintln(os.Stderr, "dworm: shell lost: container or instance stopped, or bridge did not recover")
	}
	return err
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
	var doneOnce sync.Once
	stopReaders := func() { doneOnce.Do(func() { close(m.doneCh) }) }
	defer stopReaders()

	// Create status bar
	m.statusBar = NewStatusBar(m.cfg.ContainerName)
	m.statusBar.SetSize(width, height)
	m.shadow = newShadowScreen(width, calculateShellHeight(height, m.statusBar.Height()))
	defer m.shadow.Close()
	var logUpdateCh <-chan struct{}
	m.logBuffer, logUpdateCh = NewLogBuffer(100)
	portCh := make(chan []PortMapping, 10)
	stateCh := make(chan string, 10)
	go m.readEvents(events, portCh, stateCh)

	session, err := m.startSession()
	if err != nil {
		return err
	}
	defer func() {
		if session := m.currentSession(); session != nil {
			session.Close()
		}
	}()

	// Signal handlers must be installed before entering raw mode.
	sigCh := make(chan os.Signal, 8)
	signal.Notify(sigCh, syscall.SIGWINCH, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGTSTP)
	defer signal.Stop(sigCh)

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

	go m.handleInput()
	eventsDone := make(chan struct{})
	go func() {
		defer close(eventsDone)
		m.handleEvents(sigCh, portCh, stateCh, logUpdateCh)
	}()
	defer func() {
		stopReaders()
		<-eventsDone
		// Wait for an in-flight input/layout update before terminal restoration.
		m.uiMu.Lock()
		m.clearStatusBar()
		m.uiMu.Unlock()
	}()

	for {
		exit := m.copyOutput(session)
		session.Close()
		m.setSession(nil)
		if exit == nil || exit.Error != protocol.ExecErrorBridgeLost {
			// An observed exit frame is authoritative even if a concurrent
			// instance-stop event has arrived too.
			if exit != nil && exit.Code != 0 {
				return &host.ExitError{Code: exit.Code}
			}
			return nil
		}
		m.resetChildState()
		m.writeChild([]byte("\r\n[dworm] Connection to the container lost, reconnecting...\r\n"))
		if session, err = m.waitAndRestart(); err != nil {
			return err
		}
	}
}

// startSession starts the shell on a PTY sized to the shell area.
func (m *Model) startSession() (*host.ExecSession, error) {
	m.uiMu.Lock()
	rows, cols := calculateShellHeight(m.height, m.statusBar.Height()), m.width
	m.uiMu.Unlock()
	termName := os.Getenv("TERM")
	if termName == "" {
		termName = "xterm-256color"
	}
	session, err := host.StartExec(m.cfg.SocketPath, protocol.ExecRequest{
		Argv: m.cfg.Argv,
		TTY:  &protocol.ExecTTY{Rows: rows, Cols: cols, Term: termName},
	})
	if err != nil {
		return nil, fmt.Errorf("start shell: %w", err)
	}
	m.setSession(session)
	// A device may have resized the host PTY while the start reply was pending.
	m.resizeSession()
	return session, nil
}

// waitAndRestart starts a new shell once the instance is ready again. The
// instance may still report "ready" for a moment after the exec was lost,
// so failed starts are retried.
func (m *Model) waitAndRestart() (*host.ExecSession, error) {
	timeout := m.cfg.ReconnectTimeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		if code := m.stoppedCode(); code != 0 {
			return nil, shellStopResult(code)
		}
		if m.currentState() == host.StateReady {
			session, err := m.startSession()
			if err == nil {
				m.writeChild([]byte("[dworm] Reconnected; this is a new shell.\r\n"))
				return session, nil
			}
			if !host.IsNotReady(err) {
				return nil, errShellLost
			}
		}
		select {
		case <-m.readyCh:
		case <-m.quitCh:
			return nil, shellStopResult(m.stoppedCode())
		case <-timer.C:
			return nil, errShellLost
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

func (m *Model) quit(code int) {
	m.quitOnce.Do(func() {
		m.sessionMu.Lock()
		m.quitCode = code
		m.sessionMu.Unlock()
		close(m.quitCh)
	})
}

func (m *Model) stoppedCode() int {
	m.sessionMu.Lock()
	defer m.sessionMu.Unlock()
	return m.quitCode
}

// readEvents feeds instance events to the event handler.
func (m *Model) readEvents(events *host.EventStream, portCh chan<- []PortMapping, stateCh chan<- string) {
	for {
		event, err := events.Next()
		if err != nil {
			select {
			case stateCh <- host.StateStopped:
			case <-m.doneCh:
			}
			return
		}
		switch event.Type {
		case host.EventPorts:
			ports := make([]PortMapping, len(event.Ports))
			for i, p := range event.Ports {
				ports[i] = PortMapping{ContainerPort: p.Port, LocalPort: p.LocalPort}
			}
			select {
			case portCh <- ports:
			case <-m.doneCh:
				return
			}
		case host.EventLog:
			fmt.Fprintf(m.logBuffer, "[%s] %s\n", event.Source, event.Message)
		case host.EventState:
			select {
			case stateCh <- event.State:
			case <-m.doneCh:
				return
			}
		}
	}
}

// copyOutput writes the session's output until it exits. A broken socket
// connection counts as a lost bridge unless the TUI is quitting.
func (m *Model) copyOutput(session *host.ExecSession) *protocol.ExecExit {
	var batch []byte
	flush := func() {
		if len(batch) > 0 {
			m.writeChild(batch)
			batch = batch[:0]
		}
	}
	for {
		frameType, payload, err := session.ReadFrame()
		if err != nil {
			flush()
			return &protocol.ExecExit{Code: host.ExitLost, Error: protocol.ExecErrorBridgeLost}
		}
		switch frameType {
		case protocol.FrameStdout, protocol.FrameStderr:
			batch = append(batch, payload...)
		case protocol.FrameExit:
			flush()
			return host.ParseExit(payload)
		}
		// A flood of output arrives in many small frames (PTY reads). Taking
		// what has already arrived in one go lets the shadow screen skip what
		// scrolls off; interactive output is written at once.
		if len(batch) >= maxOutputBatch || !session.FrameReady() {
			flush()
		}
	}
}

func (m *Model) handleInput() {
	buf := make([]byte, 4096)
	for {
		n, err := os.Stdin.Read(buf)
		m.uiMu.Lock()
		select {
		case <-m.doneCh:
			m.uiMu.Unlock()
			return
		default:
		}
		if err != nil {
			m.uiMu.Unlock()
			return
		}
		input := buf[:n]

		// Check if panel is expanded - any key closes it
		if m.statusBar.IsExpanded() {
			m.clearMaxStatusArea()
			m.statusBar.SetExpanded(false)
			m.resizeAndRender()
			m.uiMu.Unlock()
			m.resizeSession()
			continue
		}

		// Ctrl+G toggles the panel; input after it is dropped.
		toggle := bytes.IndexByte(input, ctrlG)
		if toggle >= 0 {
			input = input[:toggle]
		}
		if toggle >= 0 {
			m.clearMaxStatusArea()
			m.statusBar.ToggleExpanded()
			m.resizeAndRender()
		}
		m.uiMu.Unlock()
		// Never hold the layout lock across socket writes: a large paste can
		// backpressure stdin while stdout still needs to be drained/rendered.
		if session := m.currentSession(); session != nil && len(input) > 0 {
			session.Write(input)
		}
		if toggle >= 0 {
			m.resizeSession()
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
				m.uiMu.Lock()
				m.handleResize()
				m.uiMu.Unlock()
				m.resizeSession()
			case syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTSTP:
				if session := m.currentSession(); session != nil {
					key := map[os.Signal]byte{syscall.SIGINT: 0x03, syscall.SIGQUIT: 0x1c, syscall.SIGTSTP: 0x1a}[sig]
					session.Write([]byte{key})
				}
			case syscall.SIGTERM, syscall.SIGHUP:
				// Leave: closing the session hangs up the shell.
				m.quit(128 + int(sig.(syscall.Signal)))
				if session := m.currentSession(); session != nil {
					session.Close()
				}
			}

		case ports := <-portCh:
			m.uiMu.Lock()
			m.statusBar.SetPorts(ports)
			m.renderStatusBar()
			m.uiMu.Unlock()

		case state := <-stateCh:
			m.uiMu.Lock()
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
				m.quit(host.ExitLost)
				if session := m.currentSession(); session != nil {
					session.Close()
				}
			}
			m.uiMu.Unlock()

		case <-logUpdateCh:
			m.uiMu.Lock()
			m.statusBar.SetLogs(m.logBuffer.Lines())
			if m.statusBar.IsExpanded() {
				m.renderStatusBar()
			}
			m.uiMu.Unlock()
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

	m.renderStatusBar()
}

// resizeSession resizes the shell's PTY in the container.
func (m *Model) resizeSession() {
	m.uiMu.Lock()
	shellHeight, width := calculateShellHeight(m.height, m.statusBar.Height()), m.width
	m.uiMu.Unlock()
	if session := m.currentSession(); session != nil {
		session.Resize(shellHeight, width)
	}
}

func (m *Model) renderStatusBar() {
	statusHeight := m.statusBar.Height()
	startRow := m.height - statusHeight + 1

	// Render using lipgloss-styled view
	lines := bytes.Split([]byte(m.statusBar.View()), []byte("\n"))

	m.outputMu.Lock()
	defer m.outputMu.Unlock()
	m.drawOutsideShell(func(b *bytes.Buffer) {
		for i, line := range lines {
			row := startRow + i
			if row > m.height {
				break
			}
			b.WriteString(moveToRow(row) + clearLine)
			b.Write(line)
		}
	})
}

func (m *Model) clearStatusBar() {
	m.clearRows(m.height - m.statusBar.Height() + 1)
}

func (m *Model) clearMaxStatusArea() {
	// Clear the maximum possible status area
	maxHeight := m.height / 2
	if maxHeight < 5 {
		maxHeight = 5
	}
	m.clearRows(max(m.height-maxHeight+1, 1))
}

func (m *Model) clearRows(startRow int) {
	m.outputMu.Lock()
	defer m.outputMu.Unlock()
	m.drawOutsideShell(func(b *bytes.Buffer) {
		for row := startRow; row <= m.height; row++ {
			b.WriteString(moveToRow(row) + clearLine)
		}
	})
}

// drawOutsideShell writes what draw produces, with default attributes, and
// then returns the host terminal to the child's cursor state. Without a saved
// cursor of the child, the host's own save/restore does this exactly (pending
// wrap included); otherwise that slot belongs to the child and the state comes
// from the shadow screen. Called with outputMu held.
func (m *Model) drawOutsideShell(draw func(b *bytes.Buffer)) {
	var b bytes.Buffer
	keepSlot := m.shadow != nil && m.shadow.hasSaved()
	if !keepSlot {
		b.WriteString(saveCursor)
	}
	b.WriteString(sgrReset)
	draw(&b)
	if keepSlot {
		b.WriteString(m.shadow.restoreSequence())
	} else {
		b.WriteString(restoreCursor)
	}
	m.output.Write(b.Bytes())
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

	var b bytes.Buffer
	b.WriteString(setScrollRegion(1, shellHeight) + sgrReset)
	for row := shellHeight + 1; row <= totalHeight; row++ {
		b.WriteString(moveToRow(row) + clearLine)
	}
	// Setting the scroll region moved the cursor home. Return to the child's
	// cursor, which the resized shadow screen keeps inside the shell area, so
	// subsequent newlines scroll instead of one-line looping.
	if m.shadow != nil {
		m.shadow.resize(m.width, shellHeight)
		b.WriteString(m.shadow.restoreSequence())
	} else {
		b.WriteString(moveToRow(shellHeight))
	}
	m.output.Write(b.Bytes())
}

func calculateShellHeight(totalHeight, statusHeight int) int {
	shellHeight := totalHeight - statusHeight
	if shellHeight < 1 {
		return 1
	}
	return shellHeight
}

// writeChild writes the child's output (or a dworm notice that belongs to the
// shell area) and redraws the status bar when the output requires it.
func (m *Model) writeChild(data []byte) {
	m.uiMu.Lock()
	defer m.uiMu.Unlock()
	if m.writePTYOutput(data) {
		m.renderStatusBar()
	}
}

// writePTYOutput keeps incomplete ANSI sequences together, feeds the shadow
// screen, and translates the sequences that a child application means for a
// terminal of its own: the host terminal is shared with the status bar and is
// already on its alternate screen. It reports whether the status bar has to be
// drawn again. Called with uiMu held.
func (m *Model) writePTYOutput(data []byte) bool {
	m.outputMu.Lock()
	defer m.outputMu.Unlock()
	return m.writePTYOutputLocked(data)
}

func (m *Model) writePTYOutputLocked(data []byte) bool {
	data = append(m.ansiPending, data...)
	m.ansiPending = nil

	complete, pending := splitCompleteANSI(data)
	if len(pending) > 4096 {
		complete = append(complete, pending)
	} else {
		m.ansiPending = append(m.ansiPending, pending...)
	}

	redraw := false
	var out bytes.Buffer
	for i := 0; i < len(complete); {
		// Text and attributes pass through unchanged; the shadow screen
		// takes them in one go.
		n := 0
		for i+n < len(complete) && isLinearPart(complete[i+n]) {
			n++
		}
		if n > 0 {
			m.shadow.writeRun(complete[i : i+n])
			for _, part := range complete[i : i+n] {
				out.Write(part)
			}
			i += n
			continue
		}
		part := complete[i]
		wasAlt := m.shadow.emu.IsAltScreen()
		m.shadow.write(part)
		translated, r := m.translateChildSequence(part, wasAlt)
		out.Write(translated)
		redraw = redraw || r
		i++
	}
	m.output.Write(out.Bytes())
	return redraw
}

// resetChildState forgets what the previous shell left behind: a partial
// escape sequence, its alternate screen (the shell screen is repainted), its
// attributes, and its saved cursor.
func (m *Model) resetChildState() {
	m.uiMu.Lock()
	defer m.uiMu.Unlock()
	m.outputMu.Lock()
	m.ansiPending = nil
	redraw := m.writePTYOutputLocked([]byte(sgrReset + exitAltScreen))
	m.shadow.resetChild()
	m.outputMu.Unlock()
	if redraw {
		m.renderStatusBar()
	}
}

// translateChildSequence returns what to write to the host terminal for one
// part of the child's output (plain text or one complete escape sequence),
// after the shadow screen has processed it, and whether the status bar needs a
// redraw afterwards. wasAlt is the child's screen before the part.
func (m *Model) translateChildSequence(part []byte, wasAlt bool) ([]byte, bool) {
	shellHeight := calculateShellHeight(m.height, m.statusBar.Height())

	if bytes.Equal(part, []byte(fullReset)) {
		// A real RIS would leave the TUI's alternate screen (and, in some
		// terminals, erase the host's scrollback).
		return []byte(softReset + eraseScreen + setScrollRegion(1, shellHeight) + cursorHome), true
	}

	seq, ok := parseCSI(part)
	if !ok {
		return part, false
	}
	switch {
	case seq.final == 'r' && seq.prefix == 0 && seq.inter == "":
		// DECSTBM: keep the child's region above the status bar. Like the
		// original it moves the cursor home.
		return []byte(setScrollRegion(clampedRegion(seq, shellHeight))), true

	case seq.final == 'p' && seq.prefix == 0 && seq.inter == "!":
		// DECSTR resets the margins but leaves the cursor where it is; setting
		// the region moves it home.
		return []byte(string(part) + setScrollRegion(1, shellHeight) + m.shadow.cursorSequence()), true

	case (seq.final == 'h' || seq.final == 'l') && seq.prefix == '?' && seq.inter == "":
		return m.translatePrivateModes(part, seq, wasAlt)

	case seq.final == 'J' && seq.prefix == 0 && seq.inter == "":
		// Erase in display ignores the scroll region: everything but "erase
		// above" (1) also wipes the status bar.
		return part, len(seq.params) == 0 || seq.params[0] != 1

	case seq.final == 'H' && len(seq.params) == 0 && seq.prefix == 0 && seq.inter == "":
		return part, true // cursor home, usually the start of a full repaint

	case (seq.final == 'H' || seq.final == 'f' || seq.final == 'd') && seq.prefix == 0 && seq.inter == "":
		// CUP, HVP, VPA: a terminal of the child's size clamps the row to its
		// last one; the host terminal would put the cursor on the status bar
		// (top moves to row 24 of 23 on exit).
		if len(seq.params) > 0 && seq.params[0] > shellHeight {
			params := make([]string, len(seq.params))
			for i, p := range seq.params {
				params[i] = strconv.Itoa(p)
			}
			params[0] = strconv.Itoa(shellHeight)
			return []byte(csi + strings.Join(params, ";") + string(seq.final)), false
		}
	}
	return part, false
}

// clampedRegion is the scroll region (1-based, inclusive) that DECSTBM seq
// sets on a screen of the given height: out-of-range bottoms are clamped to
// the screen, an empty region is the whole screen.
func clampedRegion(seq csiSequence, height int) (int, int) {
	top, bottom := 1, height
	if len(seq.params) > 0 && seq.params[0] > 1 {
		top = seq.params[0]
	}
	if len(seq.params) > 1 && seq.params[1] > 0 && seq.params[1] < height {
		bottom = seq.params[1]
	}
	if top >= bottom {
		top, bottom = 1, height
	}
	return top, bottom
}

// translatePrivateModes emulates the child's alternate screen (modes 47, 1047
// and 1049) inside the shell area instead of forwarding it. The TUI occupies
// the host terminal's only alternate screen, so a forwarded "leave" would show
// the host's primary screen, with the cursor the host shell had before dworm
// started. Entering erases the shell area; leaving repaints it from the
// shadow screen, which also restores the cursor (and attributes) the child
// saved on entering.
func (m *Model) translatePrivateModes(part []byte, seq csiSequence, wasAlt bool) ([]byte, bool) {
	var rest []string
	alt := false
	for _, p := range seq.params {
		switch p {
		case 47, 1047, 1049:
			alt = true
		default:
			rest = append(rest, strconv.Itoa(p))
		}
	}
	if !alt {
		return part, false
	}

	var out []byte
	if len(rest) > 0 {
		out = append(out, csi+"?"+strings.Join(rest, ";")+string(seq.final)...)
	}
	switch isAlt := m.shadow.emu.IsAltScreen(); {
	case isAlt && !wasAlt:
		return append(append(out, sgrReset+eraseScreen...), m.shadow.restoreSequence()...), true
	case !isAlt && wasAlt:
		return append(out, m.shadow.repaint()...), true
	}
	return out, false
}

// csiSequence is a parsed control sequence "ESC [ prefix params inter final".
type csiSequence struct {
	prefix byte   // private marker ('?', '>', '<', '='), 0 if none
	params []int  // omitted parameters are 0
	inter  string // intermediate bytes
	final  byte
}

// parseCSI parses one complete CSI sequence. Sequences with sub-parameters
// (colons) are not parsed.
func parseCSI(part []byte) (csiSequence, bool) {
	var seq csiSequence
	if len(part) < 3 || part[0] != '\x1b' || part[1] != '[' {
		return seq, false
	}
	body := part[2 : len(part)-1]
	seq.final = part[len(part)-1]
	if seq.final < 0x40 || seq.final > 0x7e {
		return seq, false
	}
	if len(body) > 0 && body[0] >= 0x3c && body[0] <= 0x3f {
		seq.prefix = body[0]
		body = body[1:]
	}
	i := 0
	for i < len(body) && (body[i] >= '0' && body[i] <= '9' || body[i] == ';') {
		i++
	}
	paramBytes, inter := body[:i], body[i:]
	for _, b := range inter {
		if b < 0x20 || b > 0x2f {
			return seq, false
		}
	}
	if len(inter) > 0 {
		seq.inter = string(inter)
	}
	if len(paramBytes) > 0 {
		seq.params = make([]int, 0, bytes.Count(paramBytes, []byte(";"))+1)
		n := 0
		for _, d := range paramBytes {
			switch {
			case d == ';':
				seq.params = append(seq.params, n)
				n = 0
			case n < 1<<20:
				n = n*10 + int(d-'0')
			}
		}
		seq.params = append(seq.params, n)
	}
	return seq, true
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
