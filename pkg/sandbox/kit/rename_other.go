//go:build !windows

package kit

// isRetryableRenameErr is Windows-only; see rename_windows.go.
// On POSIX, directory renames succeed even when another process holds the
// directory open, so EACCES is a genuine hard error — nothing to retry.
func isRetryableRenameErr(error) bool {
	return false
}
