//go:build unix

package plan

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// lockFileExclusive attempts to acquire an exclusive advisory lock without
// blocking. The retry loop in acquireFileLock handles waiting and
// cancellation. flock locks are per open file description, so two
// FilesystemStorage instances in the same process exclude each other too.
func lockFileExclusive(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}

func unlockFile(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_UN)
}

func isLockUnavailable(err error) bool {
	return errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN)
}
