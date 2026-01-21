package tui

// PortMapping represents a port forwarding from container to host
type PortMapping struct {
	ContainerPort int
	LocalPort     int
}

// PortUpdateMsg is sent when forwarded ports change
type PortUpdateMsg struct {
	Ports []PortMapping
}

// ProcessExitedMsg is sent when the shell process exits
type ProcessExitedMsg struct {
	ExitCode int
	Error    error
}

// pollTerminalMsg triggers a terminal refresh
type pollTerminalMsg struct{}
