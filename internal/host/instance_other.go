//go:build !unix

package host

import (
	"errors"
	"os"
)

var errLocked = errors.New("lock is held by another process")

func lockFile(*os.File, bool) error {
	return errors.New("instance locking is not supported on this platform")
}
