package tui

import (
	"bytes"
	"fmt"
	"io"
	"math/rand"
	"strings"
	"testing"
)

// The batched and skipping paths (writeRun) must leave the emulator exactly
// as feeding every part on its own does.
func TestShadowScreenBatchingMatchesPlainFeeding(t *testing.T) {
	sizes := [][2]int{{80, 24}, {40, 10}, {13, 5}, {120, 40}, {7, 2}}
	for seed := int64(1); seed <= 60; seed++ {
		size := sizes[int(seed)%len(sizes)]
		t.Run(fmt.Sprintf("seed%d_%dx%d", seed, size[0], size[1]), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			chunks := randomOutput(rng, size[0])

			fast := newShadowScreen(size[0], size[1])
			plain := newShadowScreen(size[0], size[1])
			defer fast.Close()
			defer plain.Close()
			var m Model
			m.shadow = fast
			statusBar := NewStatusBar("test")
			statusBar.SetSize(size[0], size[1]+1)
			m.statusBar, m.width, m.height, m.output = statusBar, size[0], size[1]+1, io.Discard

			var pending []byte
			for _, chunk := range chunks {
				m.writePTYOutput(chunk)
				var parts [][]byte
				parts, pending = splitCompleteANSI(append(pending, chunk...))
				pending = append([]byte(nil), pending...)
				for _, part := range parts {
					plain.write(part)
				}
				compareShadows(t, fast, plain)
				if t.Failed() {
					t.Logf("chunk %q", chunk)
					return
				}
			}
		})
	}
}

// A panic in the emulator starts the shadow screen over instead of ending the
// session.
func TestShadowScreenRecoversFromEmulatorPanic(t *testing.T) {
	s := newShadowScreen(80, 23)
	defer s.Close()
	s.write([]byte("text\x1b7"))
	s.protect(func() { panic("emulator bug") })
	if s.hasSaved() || s.emu.CursorPosition().X != 0 || s.emu.Width() != 80 || s.emu.Height() != 23 {
		t.Fatalf("shadow screen not reset: saved=%v cursor=%v size=%dx%d", s.hasSaved(), s.emu.CursorPosition(), s.emu.Width(), s.emu.Height())
	}
	s.write([]byte("again"))
	if got := s.emu.CursorPosition().X; got != 5 {
		t.Fatalf("cursor after reset = %d, want 5", got)
	}
}

func compareShadows(t *testing.T, got, want *shadowScreen) {
	t.Helper()
	if g, w := got.emu.CursorPosition(), want.emu.CursorPosition(); g != w {
		t.Errorf("cursor = %v, want %v", g, w)
	}
	if g, w := got.pen[got.screen()], want.pen[want.screen()]; !g.Equal(&w) {
		t.Errorf("pen = %+v, want %+v", g, w)
	}
	for y := 0; y < want.emu.Height(); y++ {
		for x := 0; x < want.emu.Width(); x++ {
			g, w := got.emu.CellAt(x, y), want.emu.CellAt(x, y)
			if !g.Equal(w) {
				t.Errorf("cell (%d,%d) = %q %+v, want %q %+v", x, y, g.Content, g.Style, w.Content, w.Style)
				return
			}
		}
	}
	// The next character lands in the same place with the same attributes.
	got.emu.Write([]byte("#"))
	want.emu.Write([]byte("#"))
	for y := 0; y < want.emu.Height(); y++ {
		for x := 0; x < want.emu.Width(); x++ {
			if g, w := got.emu.CellAt(x, y), want.emu.CellAt(x, y); !g.Equal(w) {
				t.Errorf("after a printed character: cell (%d,%d) = %q, want %q", x, y, g.Content, w.Content)
				return
			}
		}
	}
}

// randomOutput produces chunks of shell-like output: lines of any length
// (wrapping), colors with and without backgrounds, tabs, backspaces, bare line
// feeds, titles, hyperlinks, wide characters, and now and then a sequence
// that is not batched (cursor moves, erases, saved cursor, scroll regions).
func randomOutput(rng *rand.Rand, width int) [][]byte {
	pieces := []func() string{
		func() string { return strings.Repeat("abcdefghij", width/5+1)[:rng.Intn(2*width+1)] },
		func() string { return "\r\n" },
		func() string { return "\r\n" },
		func() string { return "\r\n" },
		func() string { return "\n" },
		func() string { return "\t" },
		func() string { return "\b" },
		func() string { return "\r" },
		func() string { return "界" },
		func() string { return fmt.Sprintf("\x1b[%dm", 31+rng.Intn(7)) },
		func() string { return fmt.Sprintf("\x1b[%d;%dm", 41+rng.Intn(7), 1+rng.Intn(4)) },
		func() string { return "\x1b[0m" },
		func() string { return "\x1b[m" },
		func() string { return "\x1b[49m" },
		func() string { return "\x1b]0;title\a" },
		func() string { return "\x1b]8;;https://example.com\x1b\\link\x1b]8;;\x1b\\" },
		func() string { return strings.Repeat("line of output\r\n", rng.Intn(200)) },
		func() string {
			switch rng.Intn(10) {
			case 0:
				return fmt.Sprintf("\x1b[%d;%dH", 1+rng.Intn(30), 1+rng.Intn(100))
			case 1:
				return "\x1b[2J"
			case 2:
				return "\x1b7"
			case 3:
				return "\x1b8"
			case 4:
				return fmt.Sprintf("\x1b[%d;%dr", 1+rng.Intn(5), 3+rng.Intn(30))
			case 5:
				return "\x1b[r"
			case 6:
				return "\x1bM"
			case 7:
				return "\x1b[?1049h"
			case 8:
				return "\x1b[?1049l"
			default:
				return "\x1b[K"
			}
		},
	}
	var stream strings.Builder
	for i := 0; i < 300; i++ {
		stream.WriteString(pieces[rng.Intn(len(pieces))]())
	}
	data := []byte(stream.String())
	var chunks [][]byte
	for len(data) > 0 {
		n := min(1+rng.Intn(4000), len(data))
		chunks = append(chunks, data[:n])
		data = data[n:]
	}
	return chunks
}

// Any output must be survivable: the translation, the shadow screen, and the
// status bar drawing around it.
func FuzzWritePTYOutput(f *testing.F) {
	for _, seed := range []string{
		"\x1b[?1049h\x1b[H\x1b[2Jx\x1b[?1049l", "\x1b[?47h\x1b[?47l", "\x1b[3;30r\x1bM", "\x1b[24;1H\r\n",
		"\x1b7\x1b[10;5H\x1b8\x1b[s\x1b[u", "\x1bc\x1b[!p", "\x1b[f\x1b[d\x1b[H\x1b[J", "\x1b]8;;x\x1b\\y\x1b]8;;\x1b\\",
		"\x1b[38;5;196;48;2;1;2;3mz\x1b[m", "\x1b[?6h\x1b[?69h\x1b[2;5s\x1b[?1048h\x1b[?1048l", "界\t\b\v\f",
		strings.Repeat("line\r\n", 100),
	} {
		f.Add([]byte(seed), uint8(24), uint8(80))
	}
	f.Fuzz(func(t *testing.T, data []byte, rows, cols uint8) {
		width, height := int(cols%200)+2, int(rows%100)+2
		var output bytes.Buffer
		m := testModel(&output, width, height)
		defer m.shadow.Close()
		for len(data) > 0 {
			n := min(len(data), 1+int(data[0])%64)
			if m.writePTYOutput(data[:n]) {
				m.renderStatusBar()
			}
			data = data[n:]
		}
		m.renderStatusBar()
		m.statusBar.SetExpanded(true)
		m.updateTerminalLayout(calculateShellHeight(height, m.statusBar.Height()), height)
		m.resetChildState()
	})
}
