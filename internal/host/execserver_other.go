//go:build !linux

package host

import "net"

// checkPeer relies on the 0700 runtime directory on platforms without
// SO_PEERCRED.
func checkPeer(*net.UnixConn) error { return nil }

// waitHangup cannot distinguish a disconnect from a half-close here; raw-mode
// processes are then terminated only when writing output fails or the
// process exits.
func waitHangup(*net.UnixConn, <-chan struct{}) <-chan struct{} {
	return make(chan struct{})
}
