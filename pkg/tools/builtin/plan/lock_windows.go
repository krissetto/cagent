//go:build windows

package plan

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// maxLockRange asks LockFileEx / UnlockFileEx to cover the whole file by
// passing 0xFFFFFFFF for both the low and high 32 bits of the range.
const maxLockRange = ^uint32(0)

// lockFileExclusive attempts to acquire an exclusive lock without blocking.
// Windows has no flock, so we use LockFileEx with LOCKFILE_FAIL_IMMEDIATELY;
// the retry loop in acquireFileLock handles waiting and cancellation. Since
// only the sentinel file is locked (never a plan file), plan reads stay
// unaffected by the mandatory nature of Windows locks.
func lockFileExclusive(f *os.File) error {
	var ol windows.Overlapped
	return windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		maxLockRange,
		maxLockRange,
		&ol,
	)
}

func unlockFile(f *os.File) error {
	var ol windows.Overlapped
	return windows.UnlockFileEx(
		windows.Handle(f.Fd()),
		0,
		maxLockRange,
		maxLockRange,
		&ol,
	)
}

func isLockUnavailable(err error) bool {
	return errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}
