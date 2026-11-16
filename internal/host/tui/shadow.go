package tui

import (
	"bytes"
	"image/color"
	"io"
	"strconv"
	"strings"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
)

// shadowScreen mirrors the shell area as the child application sees it: a
// terminal emulator is fed the same output as the host terminal. The host
// terminal shows the output as it arrives; the emulator only answers what the
// host terminal cannot tell dworm: the screen to bring back when the child
// leaves its alternate screen, the cursor position, the current pen, and what
// the child saved with DECSC.
//
// The emulator keeps pen and saved cursor to itself, so they are tracked by
// handlers that run before its own (they return false to fall through).
type shadowScreen struct {
	emu    *vt.Emulator
	pen    [2]uv.Style     // current pen per screen (0 main, 1 alternate)
	saved  [2]*savedCursor // DECSC state per screen, nil if nothing was saved
	origin bool            // DECOM: cursor positions relative to the margins

	// For batched scrolling (writeRun): the scroll region per screen is the
	// whole screen, and no left/right margins (mode 69) are enabled.
	fullRegion [2]bool
	lrMargins  bool
}

type savedCursor struct {
	pos uv.Position
	pen uv.Style
}

func newShadowScreen(cols, rows int) *shadowScreen {
	s := &shadowScreen{}
	s.init(cols, rows)
	return s
}

func (s *shadowScreen) init(cols, rows int) {
	*s = shadowScreen{emu: vt.NewEmulator(cols, rows), fullRegion: [2]bool{true, true}}
	s.emu.SetScrollbackSize(1) // only the visible screen is ever repainted
	// The emulator answers queries (DA, DSR, ...) through a synchronous pipe.
	// The host terminal answers the child; drop the emulator's replies.
	go io.Copy(io.Discard, s.emu) //nolint:errcheck
	s.registerHandlers()
}

// protect runs f, which feeds the emulator. A panic in the emulator must not
// end the shell session: the shadow screen starts over (empty, so a later
// repaint shows less than the terminal did).
func (s *shadowScreen) protect(f func()) {
	defer func() {
		if r := recover(); r != nil {
			cols, rows := s.emu.Width(), s.emu.Height()
			closeEmulator(s.emu)
			s.init(cols, rows)
		}
	}()
	f()
}

func (s *shadowScreen) Close() { closeEmulator(s.emu) }

// closeEmulator ends the goroutine that drains the emulator's replies.
// Emulator.Close would race with that goroutine's Read (an unsynchronized
// flag); closing the reply pipe itself is synchronized, and the emulator
// stays usable (replies fail).
func closeEmulator(e *vt.Emulator) {
	if pw, ok := e.InputPipe().(*io.PipeWriter); ok {
		pw.Close()
	}
}

func (s *shadowScreen) screen() int {
	if s.emu.IsAltScreen() {
		return 1
	}
	return 0
}

func (s *shadowScreen) registerHandlers() {
	e := s.emu
	save := func() bool {
		i := s.screen()
		s.saved[i] = &savedCursor{pos: e.CursorPosition(), pen: s.pen[i]}
		return false
	}
	restore := func() bool {
		i := s.screen()
		s.pen[i] = uv.Style{}
		if sc := s.saved[i]; sc != nil {
			s.pen[i] = sc.pen
		}
		return false
	}
	e.RegisterCsiHandler('m', func(params ansi.Params) bool {
		uv.ReadStyle(params, &s.pen[s.screen()])
		return false
	})
	e.RegisterEscHandler('7', save)
	e.RegisterEscHandler('8', restore)
	// CSI s (SCOSC) uses the same slot. With left/right margins enabled (mode
	// 69, rare) it sets margins instead; that is not tracked. CSI u reaches
	// the emulator as ESC 8 (emulatorInput).
	e.RegisterCsiHandler('s', func(ansi.Params) bool { return save() })
	e.RegisterEscHandler('c', func() bool {
		s.pen = [2]uv.Style{}
		s.saved = [2]*savedCursor{}
		s.origin = false
		s.fullRegion = [2]bool{true, true}
		s.lrMargins = false
		return false
	})
	e.RegisterCsiHandler('r', func(params ansi.Params) bool {
		// DECSTBM, evaluated like the emulator does.
		top, _, _ := params.Param(0, 1)
		top = max(top, 1)
		bottom, _, _ := params.Param(1, e.Height())
		if bottom < 1 {
			bottom = e.Height()
		}
		if top < bottom {
			s.fullRegion[s.screen()] = top == 1 && bottom == e.Height()
		}
		return false
	})
	// DECSTR; the emulator itself does not handle it (emulatorInput resets its
	// margins and pen around it).
	e.RegisterCsiHandler(ansi.Command(0, '!', 'p'), func(ansi.Params) bool {
		s.saved[s.screen()] = nil
		s.origin = false
		return true
	})
	modes := func(set bool) vt.CsiHandler {
		return func(params ansi.Params) bool {
			alt := s.screen()
			params.ForEach(0, func(_, mode int, _ bool) {
				switch mode {
				case 6:
					s.origin = set
				case 69:
					s.lrMargins = set
				case 1048:
					if set {
						s.saved[alt] = &savedCursor{pos: e.CursorPosition(), pen: s.pen[alt]}
					} else if sc := s.saved[alt]; sc != nil {
						s.pen[alt] = sc.pen
					}
				case 1047, 1049:
					if !set {
						alt = 0
						return
					}
					if mode == 1049 {
						s.saved[alt] = &savedCursor{pos: e.CursorPosition(), pen: s.pen[alt]}
					}
					if alt == 0 {
						// The emulator carries the cursor, pen included, over.
						s.pen[1] = s.pen[0]
						alt = 1
					}
				}
			})
			return false
		}
	}
	e.RegisterCsiHandler(ansi.Command('?', 0, 'h'), modes(true))
	e.RegisterCsiHandler(ansi.Command('?', 0, 'l'), modes(false))
}

// write feeds one part of the child's output (plain text or one complete
// escape sequence) to the emulator.
func (s *shadowScreen) write(part []byte) {
	s.protect(func() { s.writePart(part) })
}

func (s *shadowScreen) writePart(part []byte) {
	in := emulatorInput(part, s.emu.Height())
	s.emu.Write(in) //nolint:errcheck
	if !s.origin && (bytes.Equal(in, []byte("\x1b8")) || isPrivateModeReset(in, 1048)) {
		// The emulator keeps a pending wrap (cursor past the last column)
		// across a cursor restore; terminals do not. Moving the cursor to
		// where it is clears it.
		pos := s.emu.CursorPosition()
		s.emu.Write([]byte(moveTo(pos.Y+1, pos.X+1))) //nolint:errcheck
	}
}

func isPrivateModeReset(part []byte, mode int) bool {
	seq, ok := parseCSI(part)
	if !ok || seq.prefix != '?' || seq.inter != "" || seq.final != 'l' {
		return false
	}
	for _, p := range seq.params {
		if p == mode {
			return true
		}
	}
	return false
}

// resetChild forgets what a lost shell saved.
func (s *shadowScreen) resetChild() {
	s.saved = [2]*savedCursor{}
}

func (s *shadowScreen) resize(cols, rows int) {
	if cols != s.emu.Width() || rows != s.emu.Height() {
		s.protect(func() {
			s.emu.Resize(cols, rows) // resets the scroll regions
			s.fullRegion = [2]bool{true, true}
		})
	}
}

// writeRun feeds consecutive parts that only print, change attributes or
// move the cursor down (see isLinearPart), with batched scrolling: the
// emulator moves the whole screen for every line it scrolls, which made it
// process large outputs at about 1 MB/s. Before each piece of up to one
// screen of line feeds, the screen is scrolled by the number of lines the
// piece will scroll, and the cursor moved up by as much; the piece's line feeds
// then move the cursor down instead of scrolling. Line wraps can only add
// advances, which scroll as usual. Every cell ends up where it would without
// batching, and the new lines get the same background, because a piece never
// spans a change of the background color.
func (s *shadowScreen) writeRun(parts [][]byte) {
	s.protect(func() { s.writeRunParts(parts) })
}

func (s *shadowScreen) writeRunParts(parts [][]byte) {
	parts = s.skipScrolledOff(parts)
	var piece []byte
	lfs := 0
	flush := func() {
		if lfs > 1 {
			s.prescroll(lfs)
		}
		s.emu.Write(piece) //nolint:errcheck
		piece, lfs = piece[:0], 0
	}
	limit := max(s.emu.Height()-1, 1)
	for _, part := range parts {
		if part[0] == '\x1b' {
			if part[1] == '[' && s.changesBackground(part) {
				flush()
				s.emu.Write(part) //nolint:errcheck
				continue
			}
			piece = append(piece, part...)
			continue
		}
		for len(part) > 0 {
			i := 0
			for ; i < len(part) && lfs < limit; i++ {
				if part[i] == '\n' || part[i] == '\v' || part[i] == '\f' {
					lfs++
				}
			}
			piece = append(piece, part[:i]...)
			part = part[i:]
			if lfs == limit {
				flush()
			}
		}
	}
	flush()
}

// skipScrolledOff drops what a run would print and then scroll off the
// screen anyway. The emulator handles a few MB/s, the bridge delivers floods of
// output a lot faster, and only the last screenful can still be visible.
//
// The run is cut after a CR LF that leaves at least a screen of line feeds
// behind it and has at least as many before it: by then the cursor is in the
// bottom row (column 0, after the CR) of a line that was just scrolled in, and
// everything else on the screen is going to be scrolled off by the rest.
// Instead of the skipped part, the emulator gets its lasting effects: OSC
// (hyperlink, title), the attributes at the cut, and the cursor position.
func (s *shadowScreen) skipScrolledOff(parts [][]byte) [][]byte {
	h := s.emu.Height()
	if !s.fullRegion[s.screen()] || s.origin || s.lrMargins {
		return parts
	}
	total := 0
	for _, part := range parts {
		if part[0] != '\x1b' {
			total += bytes.Count(part, []byte("\n")) + bytes.Count(part, []byte("\v")) + bytes.Count(part, []byte("\f"))
		}
	}
	if total < 3*h {
		return parts
	}

	// The last qualifying cut: after line feed number h or later, at most
	// total-h.
	cutPart, cutOff := -1, 0
	n := 0
	for i, part := range parts {
		if part[0] == '\x1b' {
			continue
		}
		for j, b := range part {
			if b != '\n' && b != '\v' && b != '\f' {
				continue
			}
			n++
			if n > total-h {
				break
			}
			if n >= h && b == '\n' && j > 0 && part[j-1] == '\r' {
				cutPart, cutOff = i, j+1
			}
		}
		if n > total-h {
			break
		}
	}
	if cutPart < 0 {
		return parts
	}

	// Attributes: only what follows the last plain reset counts.
	skipped := parts[:cutPart]
	pen := s.pen[s.screen()]
	from := 0
	for j := len(skipped) - 1; j >= 0; j-- {
		if p := skipped[j]; bytes.Equal(p, []byte(ansi.ResetStyle)) || bytes.Equal(p, []byte("\x1b[0m")) {
			pen, from = uv.Style{}, j+1
			break
		}
	}
	var effects []byte
	for j, part := range skipped {
		switch {
		case part[0] != '\x1b':
		case part[1] == ']':
			effects = append(effects, part...)
		case j >= from:
			applySGR(part, &pen)
		}
	}
	effects = append(effects, ansi.ResetStyle...)
	if !pen.IsZero() {
		effects = append(effects, pen.String()...)
	}
	// The screen content does not matter: the rest scrolls all of it off.
	effects = append(effects, moveTo(h, 1)...)
	s.emu.Write(effects) //nolint:errcheck

	rest := make([][]byte, 0, len(parts)-cutPart)
	if tail := parts[cutPart][cutOff:]; len(tail) > 0 {
		rest = append(rest, tail)
	}
	return append(rest, parts[cutPart+1:]...)
}

func applySGR(sgr []byte, pen *uv.Style) {
	seq, ok := parseCSI(sgr)
	if !ok {
		return
	}
	params := make(ansi.Params, len(seq.params))
	for i, p := range seq.params {
		params[i] = ansi.Param(p)
	}
	uv.ReadStyle(params, pen)
}

// prescroll scrolls the screen for a piece with lfs line feeds, if the state
// allows the arithmetic: full-screen region, absolute cursor positions, and no
// pending wrap (the cursor in the last column may be past it, which moving it
// would clear).
func (s *shadowScreen) prescroll(lfs int) {
	pos := s.emu.CursorPosition()
	h := s.emu.Height()
	if !s.fullRegion[s.screen()] || s.origin || s.lrMargins || pos.X >= s.emu.Width()-1 {
		return
	}
	// The first line feeds only move the cursor down to the bottom; it can
	// move up only as far as the top.
	n := min(lfs-(h-1-pos.Y), pos.Y)
	if n > 1 {
		s.emu.Write([]byte(csi + strconv.Itoa(n) + "S" + csi + strconv.Itoa(n) + "A")) //nolint:errcheck
	}
}

func (s *shadowScreen) changesBackground(sgr []byte) bool {
	pen := s.pen[s.screen()]
	before := pen.Bg
	applySGR(sgr, &pen)
	return !sameBackground(before, pen.Bg)
}

func sameBackground(a, b color.Color) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	r1, g1, b1, a1 := a.RGBA()
	r2, g2, b2, a2 := b.RGBA()
	return r1 == r2 && g1 == g2 && b1 == b2 && a1 == a2
}

// isLinearPart reports whether a part of the child's output can be batched by
// writeRun: text with only line-oriented control characters, SGR without
// sub-parameters, and OSC (titles, colors, hyperlinks). Everything that may
// move the cursor up or sideways to another line, or changes modes, is not.
func isLinearPart(part []byte) bool {
	if len(part) == 0 {
		return false
	}
	if part[0] != '\x1b' {
		for _, b := range part {
			if b < 0x20 && b != '\a' && b != '\b' && b != '\t' && b != '\n' && b != '\v' && b != '\f' && b != '\r' {
				return false
			}
		}
		return true
	}
	if len(part) < 2 {
		return false
	}
	if part[1] == ']' {
		return true
	}
	return isSGR(part)
}

// isSGR reports whether part is "CSI params m" without sub-parameters.
func isSGR(part []byte) bool {
	if len(part) < 3 || part[1] != '[' || part[len(part)-1] != 'm' {
		return false
	}
	for _, b := range part[2 : len(part)-1] {
		if (b < '0' || b > '9') && b != ';' {
			return false
		}
	}
	return true
}

// hasSaved reports whether the child relies on the host terminal's saved
// cursor, which dworm must then leave alone (see restoreSequence).
func (s *shadowScreen) hasSaved() bool {
	return s.saved[s.screen()] != nil
}

// cursorSequence moves the host cursor to the child's cursor and sets the
// child's pen.
func (s *shadowScreen) cursorSequence() string {
	return s.positionSequence(s.emu.CursorPosition(), s.pen[s.screen()])
}

func (s *shadowScreen) positionSequence(pos uv.Position, pen uv.Style) string {
	pos.X = min(max(pos.X, 0), s.emu.Width()-1)
	pos.Y = min(max(pos.Y, 0), s.emu.Height()-1)
	seq := moveTo(pos.Y+1, pos.X+1) + ansi.ResetStyle
	if !pen.IsZero() {
		seq += pen.String()
	}
	return seq
}

// restoreSequence brings the host terminal back to the child's state after
// dworm drew elsewhere: it puts the child's DECSC state into the host's saved
// cursor slot (which dworm's own drawing may have used, and which terminals
// share between screens and between ESC 7 and CSI s), then returns to the
// child's cursor and pen.
func (s *shadowScreen) restoreSequence() string {
	var b strings.Builder
	if sc := s.saved[s.screen()]; sc != nil {
		b.WriteString(s.positionSequence(sc.pos, sc.pen))
		b.WriteString("\x1b7")
	}
	b.WriteString(s.cursorSequence())
	return b.String()
}

// repaint draws the emulator's current screen into the shell area and
// restores the child's cursor state.
func (s *shadowScreen) repaint() string {
	var b strings.Builder
	w, h := s.emu.Width(), s.emu.Height()
	line := make(uv.Line, w)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			line[x] = uv.EmptyCell
			if c := s.emu.CellAt(x, y); c != nil {
				line[x] = *c
			}
		}
		b.WriteString(moveToRow(y + 1))
		b.WriteString(ansi.ResetStyle)
		b.WriteString(clearLine)
		b.WriteString(line.Render())
	}
	b.WriteString(s.restoreSequence())
	return b.String()
}

// emulatorInput adapts a part for the emulator (of the given height) where it
// differs from common terminals: it has no CSI u (SCORC, restore the cursor
// saved by CSI s, which is the DECSC slot), no mode 47 (the original alternate
// screen, same as 1047 for this purpose), ignores DECSTR, which resets margins
// and pen, and does not clamp scroll regions (it panics on one beyond the
// screen); it gets the region the host terminal gets.
func emulatorInput(part []byte, height int) []byte {
	seq, ok := parseCSI(part)
	if !ok {
		return part
	}
	if seq.prefix == 0 && seq.inter == "" && seq.final == 'r' {
		return []byte(setScrollRegion(clampedRegion(seq, height)))
	}
	if seq.prefix == 0 && seq.inter == "" && seq.final == 'u' && len(seq.params) == 0 {
		return []byte("\x1b8")
	}
	if seq.prefix == 0 && seq.inter == "!" && seq.final == 'p' {
		// Margins are reset without moving the cursor (DECSTBM moves it home).
		return []byte("\x1b7" + csi + "r" + "\x1b8" + string(part) + ansi.ResetStyle)
	}
	if seq.prefix != '?' || seq.inter != "" || (seq.final != 'h' && seq.final != 'l') {
		return part
	}
	has47 := false
	params := make([]string, len(seq.params))
	for i, p := range seq.params {
		if p == 47 {
			has47 = true
			p = 1047
		}
		params[i] = strconv.Itoa(p)
	}
	if !has47 {
		return part
	}
	return []byte(csi + "?" + strings.Join(params, ";") + string(seq.final))
}
