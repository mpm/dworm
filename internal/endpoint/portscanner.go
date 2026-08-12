package endpoint

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/mpm/dworm/internal/protocol"
)

const (
	// Port range to monitor
	MinPort = 1024
	MaxPort = 20000
)

// ScanListeningPorts returns a sorted list of ports that are listening with their bind addresses
func ScanListeningPorts() ([]protocol.PortInfo, error) {
	return scanListeningPorts(scanProcNetTCP)
}

func scanListeningPorts(scan func(string, bool) ([]protocol.PortInfo, error)) ([]protocol.PortInfo, error) {
	// Use a map to track unique (port, address) pairs
	type portKey struct {
		port    int
		address string
	}
	seen := make(map[portKey]struct{})
	var ports []protocol.PortInfo
	availableFamilies := 0

	families := []struct {
		path   string
		ipv6   bool
		family string
	}{
		{path: "/proc/net/tcp", family: "IPv4"},
		{path: "/proc/net/tcp6", ipv6: true, family: "IPv6"},
	}
	for _, family := range families {
		familyPorts, err := scan(family.path, family.ipv6)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("scan %s sockets: %w", family.family, err)
		}
		availableFamilies++
		for _, p := range familyPorts {
			key := portKey{p.Port, p.Address}
			if _, exists := seen[key]; !exists {
				seen[key] = struct{}{}
				ports = append(ports, p)
			}
		}
	}
	if availableFamilies == 0 {
		return nil, fmt.Errorf("no TCP socket tables available")
	}

	// Sort by port number, then by address
	sort.Slice(ports, func(i, j int) bool {
		if ports[i].Port != ports[j].Port {
			return ports[i].Port < ports[j].Port
		}
		return ports[i].Address < ports[j].Address
	})

	return ports, nil
}

// scanProcNetTCP parses /proc/net/tcp or /proc/net/tcp6
func scanProcNetTCP(path string, isIPv6 bool) ([]protocol.PortInfo, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var ports []protocol.PortInfo
	scanner := bufio.NewScanner(file)

	// Skip header line
	if !scanner.Scan() {
		return ports, nil
	}

	for scanner.Scan() {
		line := scanner.Text()
		port, address, listening, err := parseProcNetLine(line, isIPv6)
		if err != nil {
			continue
		}

		if listening && port >= MinPort && port <= MaxPort {
			ports = append(ports, protocol.PortInfo{
				Port:    port,
				Address: address,
			})
		}
	}

	return ports, scanner.Err()
}

// parseProcNetLine parses a line from /proc/net/tcp or /proc/net/tcp6
// Format: sl local_address rem_address st tx_queue rx_queue tr tm->when retrnsmt uid timeout inode
// Example: 0: 00000000:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 12345 1 ...
func parseProcNetLine(line string, isIPv6 bool) (port int, address string, listening bool, err error) {
	fields := strings.Fields(line)
	if len(fields) < 4 {
		return 0, "", false, fmt.Errorf("invalid line format")
	}

	// Parse local address (field 1)
	localAddr := fields[1]
	parts := strings.Split(localAddr, ":")
	if len(parts) != 2 {
		return 0, "", false, fmt.Errorf("invalid local address format")
	}

	// Parse the IP address from hex
	addrHex := parts[0]
	address, err = parseHexAddress(addrHex, isIPv6)
	if err != nil {
		return 0, "", false, err
	}

	// Port is in hex
	portHex := parts[1]
	portBytes, err := hex.DecodeString(portHex)
	if err != nil {
		return 0, "", false, err
	}

	// Convert port bytes to int (big endian in /proc/net/tcp)
	if len(portBytes) == 2 {
		port = int(portBytes[0])<<8 | int(portBytes[1])
	} else {
		return 0, "", false, fmt.Errorf("invalid port bytes")
	}

	// State is field 3 (hex)
	// 0A = TCP_LISTEN
	state, err := strconv.ParseUint(fields[3], 16, 8)
	if err != nil {
		return 0, "", false, err
	}

	listening = state == 0x0A

	return port, address, listening, nil
}

// parseHexAddress converts a hex-encoded address from /proc/net/tcp to a string
// IPv4 addresses are stored as 8 hex chars (4 bytes) in little-endian
// IPv6 addresses are stored as 32 hex chars (16 bytes) in little-endian per 32-bit word
func parseHexAddress(hexAddr string, isIPv6 bool) (string, error) {
	addrBytes, err := hex.DecodeString(hexAddr)
	if err != nil {
		return "", err
	}

	if isIPv6 {
		if len(addrBytes) != 16 {
			return "", fmt.Errorf("invalid IPv6 address length: %d", len(addrBytes))
		}
		// IPv6: stored as 4 little-endian 32-bit words
		// We need to reverse each 4-byte group
		for i := 0; i < 16; i += 4 {
			addrBytes[i], addrBytes[i+1], addrBytes[i+2], addrBytes[i+3] =
				addrBytes[i+3], addrBytes[i+2], addrBytes[i+1], addrBytes[i]
		}
		ip := net.IP(addrBytes)
		return ip.String(), nil
	}

	// IPv4: stored in little-endian
	if len(addrBytes) != 4 {
		return "", fmt.Errorf("invalid IPv4 address length: %d", len(addrBytes))
	}
	// Reverse the bytes (little-endian to big-endian)
	ip := net.IPv4(addrBytes[3], addrBytes[2], addrBytes[1], addrBytes[0])
	return ip.String(), nil
}

// DiffPorts returns ports that are in newPorts but not in oldPorts (added)
// and ports that are in oldPorts but not in newPorts (removed)
// Comparison is based on port number only (not address) for logging purposes
func DiffPorts(oldPorts, newPorts []protocol.PortInfo) (added, removed []int) {
	oldSet := make(map[int]struct{})
	for _, p := range oldPorts {
		oldSet[p.Port] = struct{}{}
	}

	newSet := make(map[int]struct{})
	for _, p := range newPorts {
		newSet[p.Port] = struct{}{}
	}

	for _, p := range newPorts {
		if _, exists := oldSet[p.Port]; !exists {
			added = append(added, p.Port)
		}
	}

	for _, p := range oldPorts {
		if _, exists := newSet[p.Port]; !exists {
			removed = append(removed, p.Port)
		}
	}

	return added, removed
}
