package tui

import (
	"bytes"
	"strings"
	"testing"
)

func TestUpdateTerminalLayoutLeavesCursorInsideShellRegion(t *testing.T) {
	var output bytes.Buffer
	m := &Model{output: &output}

	m.updateTerminalLayout(23, 24)

	want := setScrollRegion(1, 23) + moveToRow(24) + clearLine + moveToRow(23)
	if got := output.String(); got != want {
		t.Fatalf("layout output mismatch\n got: %q\nwant: %q", got, want)
	}
}

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

func TestWritePTYOutputRepairsTerminalResetAndAlternateScreen(t *testing.T) {
	var output bytes.Buffer
	m := testModel(&output, 80, 24)

	input := []byte("\x1bc\x1b[?1049h")
	if redraw := m.writePTYOutput(input); !redraw {
		t.Fatal("terminal state changes did not request redraw")
	}

	repair := setScrollRegion(1, 23)
	want := "\x1bc" + repair + "\x1b[?1049h" + repair
	if got := output.String(); got != want {
		t.Fatalf("repaired output mismatch\n got: %q\nwant: %q", got, want)
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

func testModel(output *bytes.Buffer, width, height int) *Model {
	statusBar := NewStatusBar("test")
	statusBar.SetSize(width, height)
	return &Model{
		output:    output,
		statusBar: statusBar,
		width:     width,
		height:    height,
	}
}
