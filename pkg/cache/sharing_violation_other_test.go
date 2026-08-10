//go:build !windows

package cache

// isTransientSharingViolation only exists on Windows, where opening a file
// can transiently fail while a concurrent rename replaces it. POSIX renames
// never make the destination unopenable.
func isTransientSharingViolation(error) bool {
	return false
}
