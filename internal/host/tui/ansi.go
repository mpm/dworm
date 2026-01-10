package tui

import (
	"fmt"
	"io"
)

// ANSI escape sequence constants
const (
	CSI = "\x1b["

	// Cursor control
	SaveCursor    = CSI + "s"
	RestoreCursor = CSI + "u"
	HideCursor    = CSI + "?25l"
	ShowCursor    = CSI + "?25h"

	// Line control
	ClearLine      = CSI + "2K" // Clear entire line
	ClearLineRight = CSI + "0K" // Clear from cursor to end of line

	// Style
	Bold    = CSI + "1m"
	Dim     = CSI + "2m"
	Inverse = CSI + "7m"
	Reset   = CSI + "0m"

	// Scroll region (set with SetScrollRegion)
	ResetScrollRegion = CSI + "r"
)

// MoveTo returns the escape sequence to move cursor to row, col (1-indexed)
func MoveTo(row, col int) string {
	return fmt.Sprintf("%s%d;%dH", CSI, row, col)
}

// MoveToRow returns the escape sequence to move cursor to start of row (1-indexed)
func MoveToRow(row int) string {
	return fmt.Sprintf("%s%d;1H", CSI, row)
}

// SetScrollRegion sets the scrolling region to rows top-bottom (1-indexed, inclusive)
func SetScrollRegion(top, bottom int) string {
	return fmt.Sprintf("%s%d;%dr", CSI, top, bottom)
}

// WriteAt writes text at a specific position without moving the cursor permanently
func WriteAt(w io.Writer, row, col int, text string) {
	fmt.Fprint(w, SaveCursor)
	fmt.Fprint(w, MoveTo(row, col))
	fmt.Fprint(w, text)
	fmt.Fprint(w, RestoreCursor)
}

// ClearRow clears an entire row and optionally writes text
func ClearRow(w io.Writer, row int, text string) {
	fmt.Fprint(w, SaveCursor)
	fmt.Fprint(w, MoveToRow(row))
	fmt.Fprint(w, ClearLine)
	if text != "" {
		fmt.Fprint(w, text)
	}
	fmt.Fprint(w, RestoreCursor)
}

// TruncateWithEllipsis truncates a string to maxLen characters, adding "..." if truncated
func TruncateWithEllipsis(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return s[:maxLen]
	}
	return s[:maxLen-3] + "..."
}

// PadRight pads a string to width with spaces on the right
func PadRight(s string, width int) string {
	if len(s) >= width {
		return s
	}
	return s + spaces(width-len(s))
}

// PadLeft pads a string to width with spaces on the left
func PadLeft(s string, width int) string {
	if len(s) >= width {
		return s
	}
	return spaces(width-len(s)) + s
}

func spaces(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = ' '
	}
	return string(b)
}
