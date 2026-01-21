package tui

import "github.com/charmbracelet/lipgloss"

var (
	// StatusBarStyle is the base style for the collapsed status bar
	StatusBarStyle = lipgloss.NewStyle().
			Reverse(true)

	// ExpandedPanelStyle is the style for the expanded panel area
	ExpandedPanelStyle = lipgloss.NewStyle().
				Reverse(true)

	// HeaderStyle is used for section headers in the expanded panel
	HeaderStyle = lipgloss.NewStyle().
			Bold(true)
)
