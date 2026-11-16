package tui

import (
	"bytes"
	"image/color"
	"io"
	"strings"
	"testing"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
)

func TestCalculateShellHeightClampsSmallTerminal(t *testing.T) {
	if got := calculateShellHeight(1, 5); got != 1 {
		t.Fatalf("calculateShellHeight() = %d, want 1", got)
	}
}

func TestRestoreLeavesAlternateScreenAndResetsTerminalDisplay(t *testing.T) {
	var output bytes.Buffer
	m := &Model{output: &output}

	m.restore()

	want := resetScrollRegion + exitAltScreen + showCursor
	if got := output.String(); got != want {
		t.Fatalf("restore output mismatch\n got: %q\nwant: %q", got, want)
	}
}

// The TUI itself runs on the host terminal's alternate screen. A child's own
// alternate screen must stay inside it, and leaving it must bring back the
// shell screen with the cursor where the shell left it. Forwarded, the "leave"
// showed the host's primary screen and the next scroll region moved the cursor
// to the top.
func TestChildAlternateScreenRestoresShellScreen(t *testing.T) {
	for _, mode := range []string{"47", "1047", "1049"} {
		t.Run(mode, func(t *testing.T) {
			m, host := newTestSession(t, 80, 24)

			m.writeChild([]byte("line 1\r\n\x1b[31mred\x1b[m line\r\n$ "))
			m.writeChild([]byte("\x1b[?" + mode + "h\x1b[H\x1b[2JAPP SCREEN\r\n\x1b[5;10Hx"))
			if got := rowText(host, 0); got != "APP SCREEN" {
				t.Fatalf("row 0 while the app runs = %q", got)
			}
			m.writeChild([]byte("\x1b[?" + mode + "l"))

			if !host.IsAltScreen() {
				t.Fatal("host terminal left the TUI's alternate screen")
			}
			for y, want := range []string{"line 1", "red line", "$", "", ""} {
				if got := rowText(host, y); got != want {
					t.Errorf("row %d = %q, want %q", y, got, want)
				}
			}
			if got := host.CursorPosition(); got != (uv.Position{X: 2, Y: 2}) {
				t.Errorf("cursor = %v, want (2,2) after the prompt", got)
			}
			if fg := host.CellAt(0, 1).Style.Fg; !sameColor(fg, ansi.Red) {
				t.Errorf("repainted color = %v, want red", fg)
			}
			if !strings.Contains(rowText(host, 23), "Ctrl+G") {
				t.Errorf("status bar row = %q", rowText(host, 23))
			}

			// The shell continues where it was.
			m.writeChild([]byte("ls\r\nfile\r\n$ "))
			if got := rowText(host, 2); got != "$ ls" {
				t.Errorf("row 2 after typing = %q", got)
			}
			if got := rowText(host, 3); got != "file" {
				t.Errorf("row 3 after typing = %q", got)
			}
		})
	}
}

// The status bar redraw used CSI s/u, the slot that also holds what the child
// saved with ESC 7, so the child's ESC 8 went to the wrong place.
func TestStatusBarRedrawKeepsChildSavedCursor(t *testing.T) {
	m, host := newTestSession(t, 80, 24)

	m.writeChild([]byte("\x1b[31mabc\x1b7\x1b[10;5H\x1b[32m"))
	m.uiMu.Lock()
	m.renderStatusBar()
	m.uiMu.Unlock()
	m.writeChild([]byte("G\x1b8R"))

	if c := host.CellAt(4, 9); c.Content != "G" || !sameColor(c.Style.Fg, ansi.Green) {
		t.Errorf("after redraw: cell (4,9) = %q %v, want green G at the cursor", c.Content, c.Style.Fg)
	}
	if c := host.CellAt(3, 0); c.Content != "R" || !sameColor(c.Style.Fg, ansi.Red) {
		t.Errorf("after ESC 8: cell (3,0) = %q %v, want red R at the saved cursor", c.Content, c.Style.Fg)
	}
}

func TestStatusBarRedrawKeepsCursorAndAttributes(t *testing.T) {
	m, host := newTestSession(t, 80, 24)

	m.writeChild([]byte("ab\x1b[1;34m"))
	m.uiMu.Lock()
	m.renderStatusBar()
	m.uiMu.Unlock()
	m.writeChild([]byte("c"))

	c := host.CellAt(2, 0)
	if c.Content != "c" || !sameColor(c.Style.Fg, ansi.Blue) || c.Style.Attrs&uv.AttrBold == 0 {
		t.Errorf("cell (2,0) = %q %+v, want bold blue c", c.Content, c.Style)
	}
	if strings.Contains(rowText(host, 23), "c") && !strings.Contains(rowText(host, 23), "Ctrl+G") {
		t.Errorf("status bar row = %q", rowText(host, 23))
	}
}

// Shrinking the shell area (expanded status panel) keeps the cursor where the
// child expects it, clamped to the new area.
func TestUpdateTerminalLayoutReturnsToChildCursor(t *testing.T) {
	m, host := newTestSession(t, 80, 24)
	m.writeChild([]byte("one\r\ntwo"))

	m.updateTerminalLayout(20, 24)
	if got := host.CursorPosition(); got != (uv.Position{X: 3, Y: 1}) {
		t.Errorf("cursor = %v, want (3,1)", got)
	}

	m.writeChild([]byte("\x1b[23;7H"))
	m.updateTerminalLayout(18, 24)
	if got := host.CursorPosition(); got != (uv.Position{X: 6, Y: 17}) {
		t.Errorf("cursor = %v, want (6,17) on the last row of the shell area", got)
	}
	// Newlines scroll the shell area instead of looping on one row.
	m.writeChild([]byte("\r\nA\r\nB"))
	if rowText(host, 16) != "A" || rowText(host, 17) != "B" {
		t.Errorf("rows 16-17 = %q %q, want A B", rowText(host, 16), rowText(host, 17))
	}
}

func TestWritePTYOutputRepairsSplitScrollRegionReset(t *testing.T) {
	var output bytes.Buffer
	m := testModel(&output, 80, 24)

	if redraw := m.writePTYOutput([]byte("before\x1b[")); redraw {
		t.Fatal("incomplete sequence requested redraw")
	}
	if got := output.String(); got != "before" {
		t.Fatalf("first output = %q, want %q", got, "before")
	}

	if redraw := m.writePTYOutput([]byte("rafter")); !redraw {
		t.Fatal("scroll region reset did not request redraw")
	}
	want := "before" + setScrollRegion(1, 23) + "after"
	if got := output.String(); got != want {
		t.Fatalf("repaired output mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestWritePTYOutputForwardsOtherPrivateModes(t *testing.T) {
	var output bytes.Buffer
	m := testModel(&output, 80, 24)

	m.writePTYOutput([]byte("\x1b[?1049;25;2004h\x1b[?25l\x1b[?1;1049l"))

	got := output.String()
	if !strings.HasPrefix(got, "\x1b[?25;2004h") || !strings.Contains(got, "\x1b[?25l\x1b[?1l") {
		t.Fatalf("other modes not forwarded: %q", got)
	}
	if strings.Contains(got, "1049") {
		t.Fatalf("alternate screen switch reached the host terminal: %q", got)
	}
}

func TestWritePTYOutputKeepsScrollRegionsAboveStatusBar(t *testing.T) {
	tests := map[string]string{
		"\x1b[r":      setScrollRegion(1, 23),
		"\x1b[;r":     setScrollRegion(1, 23),
		"\x1b[0;0r":   setScrollRegion(1, 23),
		"\x1b[1;24r":  setScrollRegion(1, 23), // the full host terminal
		"\x1b[1;999r": setScrollRegion(1, 23),
		"\x1b[5r":     setScrollRegion(5, 23),
		"\x1b[2;10r":  setScrollRegion(2, 10),
		"\x1b[30;40r": setScrollRegion(1, 23),
		"\x1b[?1;2r":  "\x1b[?1;2r", // restore private modes, not a scroll region
	}
	for input, want := range tests {
		var output bytes.Buffer
		m := testModel(&output, 80, 24)
		m.writePTYOutput([]byte(input))
		if got := output.String(); got != want {
			t.Errorf("%q: output = %q, want %q", input, got, want)
		}
	}
}

func TestWritePTYOutputTranslatesResets(t *testing.T) {
	var output bytes.Buffer
	m := testModel(&output, 80, 24)

	// A full reset must not reach the host terminal: it would leave the TUI's
	// alternate screen.
	if redraw := m.writePTYOutput([]byte("\x1b[?1049h\x1bc")); !redraw {
		t.Fatal("full reset did not request a redraw")
	}
	want := softReset + eraseScreen + setScrollRegion(1, 23) + cursorHome
	if got := output.String(); !strings.HasSuffix(got, want) || strings.Contains(got, "\x1bc") {
		t.Fatalf("full reset output = %q, want suffix %q", got, want)
	}
	if m.shadow.emu.IsAltScreen() {
		t.Fatal("full reset left the child on the alternate screen")
	}

	// A soft reset drops the margins; they are set again and the cursor goes
	// back where it was, without touching the saved cursor.
	output.Reset()
	m.writePTYOutput([]byte("ab\x1b[!p"))
	want = "ab\x1b[!p" + setScrollRegion(1, 23) + moveTo(1, 3) + sgrReset
	if got := output.String(); got != want {
		t.Fatalf("soft reset output mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestWritePTYOutputRedrawsStatusBarAfterErase(t *testing.T) {
	tests := map[string]bool{
		"\x1b[J":    true, // below the cursor: includes the status bar
		"\x1b[0J":   true,
		"\x1b[2J":   true,
		"\x1b[3J":   true,
		"\x1b[1J":   false, // above the cursor
		"\x1b[H":    true,
		"\x1b[5;1H": false,
		"\x1b[K":    false,
		"\x1b[?25l": false,
		"text":      false,
	}
	for input, want := range tests {
		var output bytes.Buffer
		m := testModel(&output, 80, 24)
		if got := m.writePTYOutput([]byte(input)); got != want {
			t.Errorf("%q: redraw = %v, want %v", input, got, want)
		}
		if got := output.String(); got != input {
			t.Errorf("%q: output = %q, want it unchanged", input, got)
		}
	}
}

// Positioning below the child's terminal clamps to its last row instead of
// reaching the status bar.
func TestWritePTYOutputClampsRowsToShellArea(t *testing.T) {
	tests := map[string]string{
		"\x1b[24;1H":  "\x1b[23;1H", // top on exit, 23-row terminal
		"\x1b[99;5H":  "\x1b[23;5H",
		"\x1b[30f":    "\x1b[23f",
		"\x1b[40d":    "\x1b[23d",
		"\x1b[23;80H": "\x1b[23;80H",
		"\x1b[5;1H":   "\x1b[5;1H",
		"\x1b[;7H":    "\x1b[;7H",
		"\x1b[?24d":   "\x1b[?24d",
		"\x1b[f":      "\x1b[f",
		"\x1b[d":      "\x1b[d",
	}
	for input, want := range tests {
		var output bytes.Buffer
		m := testModel(&output, 80, 24)
		m.writePTYOutput([]byte(input))
		if got := output.String(); got != want {
			t.Errorf("%q: output = %q, want %q", input, got, want)
		}
	}

	m, host := newTestSession(t, 80, 24)
	m.writeChild([]byte("\x1b[24;1H\r\n$ "))
	if !strings.Contains(rowText(host, 23), "Ctrl+G") {
		t.Errorf("status bar row = %q", rowText(host, 23))
	}
	if got := rowText(host, 22); got != "$" {
		t.Errorf("last shell row = %q, want the prompt", got)
	}
}

// A shell lost while an application had its alternate screen open: the shell
// screen comes back, and a partial sequence of the old shell is dropped.
func TestResetChildStateLeavesAlternateScreenOfLostShell(t *testing.T) {
	m, host := newTestSession(t, 80, 24)

	m.writeChild([]byte("$ vim\r\n\x1b[?1049h\x1b[44mVIM\x1b]0;tit"))
	m.resetChildState()
	m.writeChild([]byte("[dworm] lost\r\n"))

	if got := rowText(host, 0); got != "$ vim" {
		t.Errorf("row 0 = %q, want the shell screen", got)
	}
	if got := rowText(host, 1); got != "[dworm] lost" {
		t.Errorf("row 1 = %q", got)
	}
	if bg := host.CellAt(0, 1).Style.Bg; bg != nil {
		t.Errorf("notice background = %v, want default", bg)
	}
	if m.shadow.emu.IsAltScreen() || m.shadow.hasSaved() {
		t.Error("child state survived the reset")
	}
}

// The emulator answers queries through a synchronous pipe; unread replies
// would block the output path.
func TestShadowScreenDoesNotBlockOnQueries(t *testing.T) {
	var output bytes.Buffer
	m := testModel(&output, 80, 24)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			m.writePTYOutput([]byte("\x1b[c\x1b[6n\x1b[>c\x1b[?2026$p"))
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("writing terminal queries blocked")
	}
	if got := output.String(); !strings.HasPrefix(got, "\x1b[c\x1b[6n") {
		t.Fatalf("queries not forwarded to the host terminal: %q", got[:min(len(got), 40)])
	}
}

func TestEmulatorInputMapsLegacyAlternateScreen(t *testing.T) {
	tests := map[string]string{
		"\x1b[?47h":    "\x1b[?1047h",
		"\x1b[?25;47l": "\x1b[?25;1047l",
		"\x1b[?1049h":  "\x1b[?1049h",
		"\x1b[47m":     "\x1b[47m", // white background
		"\x1b[!p":      "\x1b7\x1b[r\x1b8\x1b[!p\x1b[m",
		"\x1b[u":       "\x1b8",
		"\x1b[3;30r":   "\x1b[3;23r", // clamped like on the host terminal
		"\x1b[r":       "\x1b[1;23r",
		"\x1b[?u":      "\x1b[?u", // keyboard protocol query
		"\x1b[1u":      "\x1b[1u",
		"text":         "text",
	}
	for input, want := range tests {
		if got := string(emulatorInput([]byte(input), 23)); got != want {
			t.Errorf("%q: emulator input = %q, want %q", input, got, want)
		}
	}
}

func TestSplitCompleteANSIWaitsForOSCCompletion(t *testing.T) {
	parts, pending := splitCompleteANSI([]byte("text\x1b]0;title"))
	if len(parts) != 1 || string(parts[0]) != "text" {
		t.Fatalf("complete parts = %q, want [text]", parts)
	}
	if got := string(pending); got != "\x1b]0;title" {
		t.Fatalf("pending = %q, want OSC sequence", got)
	}

	parts, pending = splitCompleteANSI(append(pending, '\a'))
	if len(pending) != 0 {
		t.Fatalf("pending = %q, want empty", pending)
	}
	if len(parts) != 1 || !strings.HasSuffix(string(parts[0]), "\a") {
		t.Fatalf("completed OSC = %q", parts)
	}
}

func testModel(output io.Writer, width, height int) *Model {
	statusBar := NewStatusBar("test")
	statusBar.SetSize(width, height)
	return &Model{
		output:    output,
		statusBar: statusBar,
		shadow:    newShadowScreen(width, calculateShellHeight(height, statusBar.Height())),
		width:     width,
		height:    height,
	}
}

// newTestSession sets up the layout like Model.run, with a terminal emulator
// standing in for the host terminal.
func newTestSession(t *testing.T, width, height int) (*Model, *vt.Emulator) {
	t.Helper()
	host := vt.NewEmulator(width, height)
	go io.Copy(io.Discard, host) //nolint:errcheck
	m := testModel(hostTerminal{host}, width, height)
	t.Cleanup(func() {
		m.shadow.Close()
		closeEmulator(host)
	})
	shellHeight := calculateShellHeight(height, m.statusBar.Height())
	m.writeString(enterAltScreen)
	m.writeString(setScrollRegion(1, shellHeight))
	m.clearScreen(shellHeight, height)
	m.renderStatusBar()
	return m, host
}

// hostTerminal fixes where the emulator differs from common terminals: it
// lacks CSI u (SCORC), and keeps a pending wrap across a cursor restore.
// dworm writes whole sequences per call.
type hostTerminal struct{ *vt.Emulator }

func (h hostTerminal) Write(data []byte) (int, error) {
	p := bytes.ReplaceAll(data, []byte(restoreCursor), []byte("\x1b8"))
	for len(p) > 0 {
		chunk := p
		if i := bytes.Index(p, []byte("\x1b8")); i >= 0 {
			chunk = p[:i+2]
		}
		p = p[len(chunk):]
		h.Emulator.Write(chunk) //nolint:errcheck
		if bytes.HasSuffix(chunk, []byte("\x1b8")) {
			pos := h.CursorPosition()
			h.Emulator.Write([]byte(moveTo(pos.Y+1, pos.X+1))) //nolint:errcheck
		}
	}
	return len(data), nil
}

func rowText(e *vt.Emulator, y int) string {
	var b strings.Builder
	for x := 0; x < e.Width(); x++ {
		if c := e.CellAt(x, y); c != nil {
			b.WriteString(c.Content)
		}
	}
	return strings.TrimRight(b.String(), " ")
}

func sameColor(a, b color.Color) bool {
	if a == nil || b == nil {
		return a == b
	}
	r1, g1, b1, a1 := a.RGBA()
	r2, g2, b2, a2 := b.RGBA()
	return r1 == r2 && g1 == g2 && b1 == b2 && a1 == a2
}
