package tui

import (
	"fmt"
	"sort"
	"strings"
)

// StatusBar renders the status bar at the bottom of the terminal
type StatusBar struct {
	containerName string
	ports         []PortMapping
	expanded      bool
	width         int
	height        int
}

// NewStatusBar creates a new status bar
func NewStatusBar(containerName string) *StatusBar {
	return &StatusBar{
		containerName: containerName,
	}
}

// SetSize updates the terminal dimensions
func (s *StatusBar) SetSize(width, height int) {
	s.width = width
	s.height = height
}

// SetPorts updates the list of port mappings
func (s *StatusBar) SetPorts(ports []PortMapping) {
	s.ports = ports
}

// ToggleExpanded toggles the expanded panel state
func (s *StatusBar) ToggleExpanded() {
	s.expanded = !s.expanded
}

// SetExpanded explicitly sets the expanded state
func (s *StatusBar) SetExpanded(expanded bool) {
	s.expanded = expanded
}

// IsExpanded returns whether the panel is expanded
func (s *StatusBar) IsExpanded() bool {
	return s.expanded
}

// Height returns the number of rows the status bar occupies
func (s *StatusBar) Height() int {
	if !s.expanded {
		return 1
	}
	// Expanded: header + ports + footer
	numPorts := len(s.ports)
	if numPorts == 0 {
		numPorts = 1 // "No ports forwarded" line
	}
	height := 2 + numPorts + 2 // header + ports + footer with separators
	maxHeight := s.height / 2
	if height > maxHeight {
		height = maxHeight
	}
	if height < 5 {
		height = 5
	}
	return height
}

// View returns the rendered status bar
func (s *StatusBar) View() string {
	if s.expanded {
		return s.renderExpanded()
	}
	return s.renderCollapsed()
}

func (s *StatusBar) renderCollapsed() string {
	// Build the status line content
	left := s.containerName
	right := "Ctrl+G: help"

	// Build ports section
	portsStr := s.formatPortsCompact()

	// Calculate available space for ports
	minSpacing := 4
	fixedWidth := len(left) + len(right) + minSpacing

	availableForPorts := s.width - fixedWidth
	if availableForPorts < 0 {
		availableForPorts = 0
	}

	if len(portsStr) > availableForPorts && availableForPorts > 0 {
		portsStr = truncateWithEllipsis(portsStr, availableForPorts)
	}

	// Build the full line
	var line strings.Builder
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
	padding := s.width - currentLen - len(right)
	if padding > 0 {
		line.WriteString(spaces(padding))
	}
	line.WriteString(right)

	// Pad to full width if needed
	lineLen := currentLen + len(right)
	if padding > 0 {
		lineLen += padding
	}
	if lineLen < s.width {
		line.WriteString(spaces(s.width - lineLen))
	}

	return StatusBarStyle.Width(s.width).Render(line.String())
}

func (s *StatusBar) renderExpanded() string {
	height := s.Height()
	var lines []string

	// Header
	header := fmt.Sprintf(" Forwarded Ports ─ %s ", s.containerName)
	headerLine := s.buildBoxLine(header, '─')
	lines = append(lines, ExpandedPanelStyle.Width(s.width).Render(headerLine))

	// Port list
	portLines := s.formatPortsExpanded(height - 3)
	for _, line := range portLines {
		paddedLine := padRight("  "+line, s.width)
		lines = append(lines, ExpandedPanelStyle.Width(s.width).Render(paddedLine))
	}

	// Fill remaining rows if any
	usedRows := 1 + len(portLines)
	for i := usedRows; i < height-1; i++ {
		lines = append(lines, ExpandedPanelStyle.Width(s.width).Render(spaces(s.width)))
	}

	// Footer
	footer := " Press any key to close "
	footerLine := s.buildBoxLine(footer, '─')
	lines = append(lines, ExpandedPanelStyle.Width(s.width).Render(footerLine))

	return strings.Join(lines, "\n")
}

func (s *StatusBar) buildBoxLine(text string, fillChar rune) string {
	textLen := len(text)
	remaining := s.width - textLen
	if remaining < 0 {
		return truncateWithEllipsis(text, s.width)
	}
	leftPad := remaining / 2
	rightPad := remaining - leftPad
	return strings.Repeat(string(fillChar), leftPad) + text + strings.Repeat(string(fillChar), rightPad)
}

func (s *StatusBar) formatPortsCompact() string {
	if len(s.ports) == 0 {
		return "Ports: none"
	}

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

func (s *StatusBar) formatPortsExpanded(maxLines int) []string {
	if len(s.ports) == 0 {
		return []string{"No ports forwarded"}
	}

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

func truncateWithEllipsis(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return s[:maxLen]
	}
	return s[:maxLen-3] + "..."
}

func padRight(s string, width int) string {
	if len(s) >= width {
		return s
	}
	return s + spaces(width-len(s))
}

func spaces(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat(" ", n)
}
