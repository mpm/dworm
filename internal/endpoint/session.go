package endpoint

import (
	"bytes"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// sessionMembers returns the live (non-zombie) processes of a session.
func sessionMembers(sid int) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	want := strconv.Itoa(sid)
	var pids []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		data, err := os.ReadFile("/proc/" + entry.Name() + "/stat")
		if err != nil {
			continue
		}
		// "pid (comm) state ppid pgrp session ...": comm may contain spaces
		// and parentheses, so fields are counted from the last ')'.
		i := bytes.LastIndexByte(data, ')')
		if i < 0 {
			continue
		}
		fields := strings.Fields(string(data[i+1:]))
		if len(fields) > 3 && fields[3] == want && fields[0] != "Z" {
			pids = append(pids, pid)
		}
	}
	return pids
}

// signalSession sends sig to every process of a session, including members
// that job control moved to other process groups.
func signalSession(sid int, sig syscall.Signal) {
	syscall.Kill(-sid, sig)
	for _, pid := range sessionMembers(sid) {
		syscall.Kill(pid, sig)
	}
}

func sessionAlive(sid int) bool {
	return len(sessionMembers(sid)) > 0
}
