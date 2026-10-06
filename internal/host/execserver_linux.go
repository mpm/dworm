package host

import (
	"fmt"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// checkPeer allows only clients running as the same user.
func checkPeer(conn *net.UnixConn) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var cred *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return err
	}
	if credErr != nil {
		return fmt.Errorf("read peer credentials: %w", credErr)
	}
	if int(cred.Uid) != os.Getuid() {
		return fmt.Errorf("peer uid %d is not %d", cred.Uid, os.Getuid())
	}
	return nil
}

// waitHangup returns a channel that is closed once the peer has closed the
// connection completely. Linux reports POLLHUP only when both directions are
// shut down, which distinguishes a disconnect from a half-close (stdin EOF).
func waitHangup(conn *net.UnixConn, stop <-chan struct{}) <-chan struct{} {
	hangup := make(chan struct{})
	raw, err := conn.SyscallConn()
	if err != nil {
		return hangup
	}
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			hup := false
			if err := raw.Control(func(fd uintptr) {
				fds := []unix.PollFd{{Fd: int32(fd)}}
				n, err := unix.Poll(fds, 250)
				hup = err == nil && n > 0 && fds[0].Revents&(unix.POLLHUP|unix.POLLERR) != 0
			}); err != nil {
				return
			}
			if hup {
				close(hangup)
				return
			}
		}
	}()
	return hangup
}
