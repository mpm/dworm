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
	logs          []string
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

// SetLogs updates the stored log lines
func (s *StatusBar) SetLogs(logs []string) {
	s.logs = logs
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
	// Expanded: header + ports + separator + log header + logs + footer
	numPorts := len(s.ports)
	if numPorts == 0 {
		numPorts = 1 // "No ports forwarded" line
	}
	numLogs := len(s.logs)
	if numLogs == 0 {
		numLogs = 1 // "No log messages" line
	}
	// header + ports + blank separator + log header + logs + footer
	height := 1 + numPorts + 1 + 1 + numLogs + 1
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

	// Calculate space: total height - header(1) - footer(1) = content area
	contentRows := height - 2
	// Reserve at least 2 rows for logs section (header + 1 line)
	maxPortRows := contentRows - 3 // separator + log header + at least 1 log line
	if maxPortRows < 1 {
		maxPortRows = 1
	}

	// Port list
	portLines := s.formatPortsExpanded(maxPortRows)
	for _, line := range portLines {
		paddedLine := padRight("  "+line, s.width)
		lines = append(lines, ExpandedPanelStyle.Width(s.width).Render(paddedLine))
	}

	// Separator + log header
	logHeader := " Logs "
	logHeaderLine := s.buildBoxLine(logHeader, '─')
	lines = append(lines, ExpandedPanelStyle.Width(s.width).Render(logHeaderLine))

	// Log lines - fill remaining space
	usedRows := 1 + len(portLines) + 1 // header + ports + log header
	logRows := height - usedRows - 1    // minus footer
	if logRows < 1 {
		logRows = 1
	}
	logLines := s.formatLogsExpanded(logRows)
	for _, line := range logLines {
		paddedLine := padRight("  "+line, s.width)
		lines = append(lines, ExpandedPanelStyle.Width(s.width).Render(paddedLine))
	}

	// Fill remaining rows if any
	usedRows = 1 + len(portLines) + 1 + len(logLines)
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

func (s *StatusBar) formatLogsExpanded(maxLines int) []string {
	if len(s.logs) == 0 {
		return []string{"No log messages"}
	}

	// Show the most recent lines that fit
	start := 0
	if len(s.logs) > maxLines {
		start = len(s.logs) - maxLines
	}

	lines := make([]string, 0, maxLines)
	for i := start; i < len(s.logs); i++ {
		line := s.logs[i]
		// Truncate long lines to fit terminal width (minus indent)
		maxLen := s.width - 4
		if maxLen > 0 && len(line) > maxLen {
			line = truncateWithEllipsis(line, maxLen)
		}
		lines = append(lines, line)
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
