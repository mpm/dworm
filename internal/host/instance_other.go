//go:build !unix

package host

import (
	"errors"
	"os"
	"syscall"
)

var errLocked = errors.New("lock is held by another process")

func lockFile(*os.File, bool) error {
	return errors.New("instance locking is not supported on this platform")
}

func redirectStdio(*os.File) error { return nil }

func detachedAttrs() *syscall.SysProcAttr { return nil }
