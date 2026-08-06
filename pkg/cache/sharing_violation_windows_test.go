//go:build windows

package cache

import (
	"errors"

	"golang.org/x/sys/windows"
)

// isTransientSharingViolation reports whether a file open failed because a
// concurrent rename-over-existing briefly held the destination. During
// MoveFileEx(MOVEFILE_REPLACE_EXISTING) the target can be transiently
// unopenable, so a reader racing Store's temp-file+rename publish sees
// ERROR_SHARING_VIOLATION; that is not a torn write.
func isTransientSharingViolation(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}
