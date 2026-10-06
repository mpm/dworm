//go:build unix

package host

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

var errLocked = errors.New("lock is held by another process")

func lockFile(file *os.File, exclusive bool) error {
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	for {
		err := syscall.Flock(int(file.Fd()), how|syscall.LOCK_NB)
		if err == syscall.EINTR {
			continue
		}
		if err == syscall.EWOULDBLOCK {
			return errLocked
		}
		if err == nil && !exclusive {
			// A probe only tests the lock; release it immediately.
			syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		}
		return err
	}
}

// redirectStdio points stdout and stderr at file.
func redirectStdio(file *os.File) error {
	if err := unix.Dup2(int(file.Fd()), 1); err != nil {
		return err
	}
	return unix.Dup2(int(file.Fd()), 2)
}

// detachedAttrs starts a process in a new session, without a controlling
// terminal.
func detachedAttrs() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
