package endpoint

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

const (
	// Port range to monitor
	MinPort = 1024
	MaxPort = 20000
)

// ScanListeningPorts returns a sorted list of ports that are listening
func ScanListeningPorts() ([]int, error) {
	ports := make(map[int]struct{})

	// Scan IPv4
	if err := scanProcNetTCP("/proc/net/tcp", ports); err != nil {
		// Not fatal - file might not exist
		_ = err
	}

	// Scan IPv6
	if err := scanProcNetTCP("/proc/net/tcp6", ports); err != nil {
		// Not fatal - file might not exist
		_ = err
	}

	// Convert to sorted slice
	result := make([]int, 0, len(ports))
	for port := range ports {
		result = append(result, port)
	}
	sort.Ints(result)

	return result, nil
}

// scanProcNetTCP parses /proc/net/tcp or /proc/net/tcp6
func scanProcNetTCP(path string, ports map[int]struct{}) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)

	// Skip header line
	if !scanner.Scan() {
		return nil
	}

	for scanner.Scan() {
		line := scanner.Text()
		port, listening, err := parseProcNetLine(line)
		if err != nil {
			continue
		}

		if listening && port >= MinPort && port <= MaxPort {
			ports[port] = struct{}{}
		}
	}

	return scanner.Err()
}

// parseProcNetLine parses a line from /proc/net/tcp
// Format: sl local_address rem_address st tx_queue rx_queue tr tm->when retrnsmt uid timeout inode
// Example: 0: 00000000:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 12345 1 ...
func parseProcNetLine(line string) (port int, listening bool, err error) {
	fields := strings.Fields(line)
	if len(fields) < 4 {
		return 0, false, fmt.Errorf("invalid line format")
	}

	// Parse local address (field 1)
	localAddr := fields[1]
	parts := strings.Split(localAddr, ":")
	if len(parts) != 2 {
		return 0, false, fmt.Errorf("invalid local address format")
	}

	// Port is in hex
	portHex := parts[1]
	portBytes, err := hex.DecodeString(portHex)
	if err != nil {
		return 0, false, err
	}

	// Convert port bytes to int (big endian in /proc/net/tcp)
	if len(portBytes) == 2 {
		port = int(portBytes[0])<<8 | int(portBytes[1])
	} else {
		return 0, false, fmt.Errorf("invalid port bytes")
	}

	// State is field 3 (hex)
	// 0A = TCP_LISTEN
	state, err := strconv.ParseUint(fields[3], 16, 8)
	if err != nil {
		return 0, false, err
	}

	listening = state == 0x0A

	return port, listening, nil
}

// DiffPorts returns ports that are in newPorts but not in oldPorts (added)
// and ports that are in oldPorts but not in newPorts (removed)
func DiffPorts(oldPorts, newPorts []int) (added, removed []int) {
	oldSet := make(map[int]struct{})
	for _, p := range oldPorts {
		oldSet[p] = struct{}{}
	}

	newSet := make(map[int]struct{})
	for _, p := range newPorts {
		newSet[p] = struct{}{}
	}

	for _, p := range newPorts {
		if _, exists := oldSet[p]; !exists {
			added = append(added, p)
		}
	}

	for _, p := range oldPorts {
		if _, exists := newSet[p]; !exists {
			removed = append(removed, p)
		}
	}

	return added, removed
}
