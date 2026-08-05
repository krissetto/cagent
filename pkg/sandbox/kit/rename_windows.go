//go:build windows

package kit

import (
	"errors"
	"syscall"
)

// isRetryableRenameErr reports whether err from renaming a directory aside
// during promote is a transient Windows failure worth retrying.
//
// Windows refuses to rename a directory while another process or goroutine
// holds an open handle to it, surfacing as ERROR_ACCESS_DENIED. Concurrent
// Builds for the same agent hit exactly this: one goroutine holds finalDir
// open while another tries to retire it. The condition clears once the
// other goroutine releases the handle, so promote retries rather than
// failing hard. On POSIX, renames succeed even with open handles, so the
// non-Windows build always returns false.
func isRetryableRenameErr(err error) bool {
	return errors.Is(err, syscall.ERROR_ACCESS_DENIED)
}
