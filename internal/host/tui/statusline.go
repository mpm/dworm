package tui

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

// PortMapping represents a port forwarding from container to host
type PortMapping struct {
	ContainerPort int
	LocalPort     int
}

// StatusLine renders a status bar at the bottom of the terminal
type StatusLine struct {
	mu            sync.Mutex
	containerName string
	ports         []PortMapping
	termWidth     int
	termHeight    int
	expanded      bool
	out           *TermWriter
}

// NewStatusLine creates a new status line renderer
func NewStatusLine(out *TermWriter, containerName string, width, height int) *StatusLine {
	return &StatusLine{
		containerName: containerName,
		termWidth:     width,
		termHeight:    height,
		out:           out,
	}
}

// SetTerminalSize updates the terminal dimensions
func (s *StatusLine) SetTerminalSize(width, height int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.termWidth = width
	s.termHeight = height
}

// UpdatePorts updates the list of port mappings
func (s *StatusLine) UpdatePorts(ports []PortMapping) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ports = ports
}

// ToggleExpanded toggles the expanded panel state
func (s *StatusLine) ToggleExpanded() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expanded = !s.expanded
}

// SetExpanded explicitly sets the expanded state
func (s *StatusLine) SetExpanded(expanded bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expanded = expanded
}

// IsExpanded returns whether the panel is expanded
func (s *StatusLine) IsExpanded() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.expanded
}

// Height returns the number of rows the status line occupies
func (s *StatusLine) Height() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.heightLocked()
}

func (s *StatusLine) heightLocked() int {
	if !s.expanded {
		return 1
	}
	// Expanded: header + separator + ports + separator + footer
	// Minimum 5 rows, maximum half the terminal
	numPorts := len(s.ports)
	if numPorts == 0 {
		numPorts = 1 // "No ports forwarded" line
	}
	height := 2 + numPorts + 2 // header + ports + footer with separators
	maxHeight := s.termHeight / 2
	if height > maxHeight {
		height = maxHeight
	}
	if height < 5 {
		height = 5
	}
	return height
}

// Render draws the status line at the bottom of the terminal
func (s *StatusLine) Render() {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Use atomic write to prevent interleaving with PTY output
	s.out.Atomic(func(w io.Writer) {
		if s.expanded {
			s.renderExpandedTo(w)
		} else {
			s.renderCollapsedTo(w)
		}
	})
}

func (s *StatusLine) renderCollapsedTo(w io.Writer) {
	row := s.termHeight

	// Build the status line content
	left := s.containerName
	right := "Ctrl+G: help"

	// Build ports section
	portsStr := s.formatPortsCompact()

	// Calculate available space for ports
	// Format: "container  Ports: ...    Ctrl+G: help"
	minSpacing := 4 // 2 spaces on each side of ports
	fixedWidth := len(left) + len(right) + minSpacing

	availableForPorts := s.termWidth - fixedWidth
	if availableForPorts < 0 {
		availableForPorts = 0
	}

	if len(portsStr) > availableForPorts && availableForPorts > 0 {
		portsStr = TruncateWithEllipsis(portsStr, availableForPorts)
	}

	// Build the full line
	var line strings.Builder
	line.WriteString(Inverse)
	line.WriteString(left)

	if portsStr != "" {
		line.WriteString("  ")
		line.WriteString(portsStr)
	}

	// Calculate padding to push right section to the right
	currentLen := len(left) + 2 + len(portsStr)
	if portsStr == "" {
		currentLen = len(left)
	}
	padding := s.termWidth - currentLen - len(right)
	if padding > 0 {
		line.WriteString(spaces(padding))
	}
	line.WriteString(right)

	// Pad to full width if needed
	lineLen := currentLen + len(right)
	if padding > 0 {
		lineLen += padding
	}
	if lineLen < s.termWidth {
		line.WriteString(spaces(s.termWidth - lineLen))
	}

	line.WriteString(Reset)

	clearRowTo(w, row, line.String())
}

func (s *StatusLine) renderExpandedTo(w io.Writer) {
	height := s.heightLocked()
	startRow := s.termHeight - height + 1

	// Header
	header := fmt.Sprintf(" Forwarded Ports ─ %s ", s.containerName)
	headerLine := s.buildBoxLine(header, '─')
	clearRowTo(w, startRow, Inverse+headerLine+Reset)

	// Port list
	portLines := s.formatPortsExpanded(height - 3) // -3 for header, footer, close hint
	for i, line := range portLines {
		row := startRow + 1 + i
		paddedLine := PadRight("  "+line, s.termWidth)
		clearRowTo(w, row, Inverse+paddedLine+Reset)
	}

	// Fill remaining rows if any
	usedRows := 1 + len(portLines)
	for row := startRow + usedRows; row < s.termHeight; row++ {
		clearRowTo(w, row, Inverse+spaces(s.termWidth)+Reset)
	}

	// Footer
	footer := " Press any key to close "
	footerLine := s.buildBoxLine(footer, '─')
	clearRowTo(w, s.termHeight, Inverse+footerLine+Reset)
}

func (s *StatusLine) buildBoxLine(text string, fillChar rune) string {
	textLen := len(text)
	remaining := s.termWidth - textLen
	if remaining < 0 {
		return TruncateWithEllipsis(text, s.termWidth)
	}
	leftPad := remaining / 2
	rightPad := remaining - leftPad
	return strings.Repeat(string(fillChar), leftPad) + text + strings.Repeat(string(fillChar), rightPad)
}

func (s *StatusLine) formatPortsCompact() string {
	if len(s.ports) == 0 {
		return "Ports: none"
	}

	// Sort ports for consistent display
	sorted := make([]PortMapping, len(s.ports))
	copy(sorted, s.ports)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].ContainerPort < sorted[j].ContainerPort
	})

	var parts []string
	for _, p := range sorted {
		if p.ContainerPort == p.LocalPort {
			parts = append(parts, fmt.Sprintf("%d", p.ContainerPort))
		} else {
			parts = append(parts, fmt.Sprintf("%d→%d", p.LocalPort, p.ContainerPort))
		}
	}

	return "Ports: " + strings.Join(parts, " ")
}

func (s *StatusLine) formatPortsExpanded(maxLines int) []string {
	if len(s.ports) == 0 {
		return []string{"No ports forwarded"}
	}

	// Sort ports for consistent display
	sorted := make([]PortMapping, len(s.ports))
	copy(sorted, s.ports)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].ContainerPort < sorted[j].ContainerPort
	})

	var lines []string
	for i, p := range sorted {
		if i >= maxLines-1 && len(sorted) > maxLines {
			lines = append(lines, fmt.Sprintf("... and %d more", len(sorted)-i))
			break
		}
		lines = append(lines, fmt.Sprintf("localhost:%d → container:%d", p.LocalPort, p.ContainerPort))
	}

	return lines
}

// Clear removes the status line from the terminal
func (s *StatusLine) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()

	height := s.heightLocked()
	startRow := s.termHeight - height + 1

	// Use atomic write to prevent interleaving
	s.out.Atomic(func(w io.Writer) {
		for row := startRow; row <= s.termHeight; row++ {
			clearRowTo(w, row, "")
		}
	})
}

// clearRowTo clears an entire row and optionally writes text (internal helper)
func clearRowTo(w io.Writer, row int, text string) {
	fmt.Fprint(w, SaveCursor)
	fmt.Fprint(w, MoveToRow(row))
	fmt.Fprint(w, ClearLine)
	if text != "" {
		fmt.Fprint(w, text)
	}
	fmt.Fprint(w, RestoreCursor)
}
