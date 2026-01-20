package endpoint

import (
	"reflect"
	"testing"

	"github.com/mpm/dworm/internal/protocol"
)

func TestParseProcNetLine(t *testing.T) {
	tests := []struct {
		name       string
		line       string
		isIPv6     bool
		wantPort   int
		wantAddr   string
		wantListen bool
		wantErr    bool
	}{
		{
			name:       "listening on port 8080 all interfaces",
			line:       "   0: 00000000:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 12345 1",
			isIPv6:     false,
			wantPort:   8080,
			wantAddr:   "0.0.0.0",
			wantListen: true,
		},
		{
			name:       "listening on port 3000 all interfaces",
			line:       "   1: 00000000:0BB8 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 12346 1",
			isIPv6:     false,
			wantPort:   3000,
			wantAddr:   "0.0.0.0",
			wantListen: true,
		},
		{
			name:       "established connection (not listening)",
			line:       "   2: 0100007F:1F90 0100007F:AB12 01 00000000:00000000 00:00000000 00000000  1000        0 12347 1",
			isIPv6:     false,
			wantPort:   8080,
			wantAddr:   "127.0.0.1",
			wantListen: false,
		},
		{
			name:       "time wait state (not listening)",
			line:       "   3: 0100007F:1F90 0100007F:AB12 06 00000000:00000000 00:00000000 00000000  1000        0 12348 1",
			isIPv6:     false,
			wantPort:   8080,
			wantAddr:   "127.0.0.1",
			wantListen: false,
		},
		{
			name:       "port 22 (SSH) on all interfaces",
			line:       "   4: 00000000:0016 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 12349 1",
			isIPv6:     false,
			wantPort:   22,
			wantAddr:   "0.0.0.0",
			wantListen: true,
		},
		{
			name:       "high port 65535",
			line:       "   5: 00000000:FFFF 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 12350 1",
			isIPv6:     false,
			wantPort:   65535,
			wantAddr:   "0.0.0.0",
			wantListen: true,
		},
		{
			name:       "localhost binding",
			line:       "   6: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 12351 1",
			isIPv6:     false,
			wantPort:   8080,
			wantAddr:   "127.0.0.1",
			wantListen: true,
		},
		{
			name:    "invalid line - too few fields",
			line:    "0: 00000000:1F90",
			isIPv6:  false,
			wantErr: true,
		},
		{
			name:    "invalid line - empty",
			line:    "",
			isIPv6:  false,
			wantErr: true,
		},
		{
			name:    "invalid local address format",
			line:    "   0: 00000000 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 12345 1",
			isIPv6:  false,
			wantErr: true,
		},
		{
			name:    "invalid port hex",
			line:    "   0: 00000000:ZZZZ 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 12345 1",
			isIPv6:  false,
			wantErr: true,
		},
		{
			name:    "invalid state hex",
			line:    "   0: 00000000:1F90 00000000:0000 ZZ 00000000:00000000 00:00000000 00000000  1000        0 12345 1",
			isIPv6:  false,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			port, addr, listening, err := parseProcNetLine(tt.line, tt.isIPv6)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseProcNetLine() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				if port != tt.wantPort {
					t.Errorf("parseProcNetLine() port = %v, want %v", port, tt.wantPort)
				}
				if addr != tt.wantAddr {
					t.Errorf("parseProcNetLine() addr = %v, want %v", addr, tt.wantAddr)
				}
				if listening != tt.wantListen {
					t.Errorf("parseProcNetLine() listening = %v, want %v", listening, tt.wantListen)
				}
			}
		})
	}
}

func TestDiffPorts(t *testing.T) {
	// Helper to create PortInfo slice from port numbers
	makePortInfos := func(ports ...int) []protocol.PortInfo {
		result := make([]protocol.PortInfo, len(ports))
		for i, p := range ports {
			result[i] = protocol.PortInfo{Port: p, Address: "0.0.0.0"}
		}
		return result
	}

	tests := []struct {
		name        string
		oldPorts    []protocol.PortInfo
		newPorts    []protocol.PortInfo
		wantAdded   []int
		wantRemoved []int
	}{
		{
			name:        "empty to some ports",
			oldPorts:    []protocol.PortInfo{},
			newPorts:    makePortInfos(8080, 3000),
			wantAdded:   []int{8080, 3000},
			wantRemoved: nil,
		},
		{
			name:        "some ports to empty",
			oldPorts:    makePortInfos(8080, 3000),
			newPorts:    []protocol.PortInfo{},
			wantAdded:   nil,
			wantRemoved: []int{8080, 3000},
		},
		{
			name:        "same ports",
			oldPorts:    makePortInfos(8080, 3000),
			newPorts:    makePortInfos(8080, 3000),
			wantAdded:   nil,
			wantRemoved: nil,
		},
		{
			name:        "added only",
			oldPorts:    makePortInfos(8080),
			newPorts:    makePortInfos(8080, 3000),
			wantAdded:   []int{3000},
			wantRemoved: nil,
		},
		{
			name:        "removed only",
			oldPorts:    makePortInfos(8080, 3000),
			newPorts:    makePortInfos(8080),
			wantAdded:   nil,
			wantRemoved: []int{3000},
		},
		{
			name:        "both added and removed",
			oldPorts:    makePortInfos(8080, 3000),
			newPorts:    makePortInfos(8080, 5432),
			wantAdded:   []int{5432},
			wantRemoved: []int{3000},
		},
		{
			name:        "completely different",
			oldPorts:    makePortInfos(8080, 3000),
			newPorts:    makePortInfos(5432, 6379),
			wantAdded:   []int{5432, 6379},
			wantRemoved: []int{8080, 3000},
		},
		{
			name:        "both empty",
			oldPorts:    []protocol.PortInfo{},
			newPorts:    []protocol.PortInfo{},
			wantAdded:   nil,
			wantRemoved: nil,
		},
		{
			name:        "nil inputs",
			oldPorts:    nil,
			newPorts:    nil,
			wantAdded:   nil,
			wantRemoved: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			added, removed := DiffPorts(tt.oldPorts, tt.newPorts)
			if !reflect.DeepEqual(added, tt.wantAdded) {
				t.Errorf("DiffPorts() added = %v, want %v", added, tt.wantAdded)
			}
			if !reflect.DeepEqual(removed, tt.wantRemoved) {
				t.Errorf("DiffPorts() removed = %v, want %v", removed, tt.wantRemoved)
			}
		})
	}
}

func TestPortRangeConstants(t *testing.T) {
	if MinPort != 1024 {
		t.Errorf("MinPort = %d, want 1024", MinPort)
	}
	if MaxPort != 20000 {
		t.Errorf("MaxPort = %d, want 20000", MaxPort)
	}
	if MinPort >= MaxPort {
		t.Error("MinPort should be less than MaxPort")
	}
}

func TestParseProcNetLineIPv6(t *testing.T) {
	// IPv6 lines have longer address but same format
	tests := []struct {
		name       string
		line       string
		wantPort   int
		wantAddr   string
		wantListen bool
	}{
		{
			name:       "IPv6 listening on port 8080 all interfaces",
			line:       "   0: 00000000000000000000000000000000:1F90 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 12345 1",
			wantPort:   8080,
			wantAddr:   "::",
			wantListen: true,
		},
		{
			name:       "IPv6 loopback listening on 3000",
			line:       "   1: 00000000000000000000000001000000:0BB8 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 12346 1",
			wantPort:   3000,
			wantAddr:   "::1",
			wantListen: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			port, addr, listening, err := parseProcNetLine(tt.line, true)
			if err != nil {
				t.Fatalf("parseProcNetLine() error = %v", err)
			}
			if port != tt.wantPort {
				t.Errorf("parseProcNetLine() port = %v, want %v", port, tt.wantPort)
			}
			if addr != tt.wantAddr {
				t.Errorf("parseProcNetLine() addr = %v, want %v", addr, tt.wantAddr)
			}
			if listening != tt.wantListen {
				t.Errorf("parseProcNetLine() listening = %v, want %v", listening, tt.wantListen)
			}
		})
	}
}
