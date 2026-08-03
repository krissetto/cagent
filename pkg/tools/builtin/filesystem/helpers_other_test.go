//go:build !windows

package filesystem

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// Post-edit commands run under $SHELL (or /bin/sh) on non-Windows
// platforms (shellpath.DetectShell). These helpers are mirrored for
// PowerShell in helpers_windows_test.go so the tests validate the same
// contracts on every platform.

// postEditMarkerCmd returns a command creating an empty "<path>.formatted"
// marker file, where <path> arrives in the "file" environment variable
// injected by runPostEditCommands.
func postEditMarkerCmd() string {
	return `touch "$file.formatted"`
}

// makeUnreadable revokes read access to path for the rest of the test.
// POSIX modes express this directly; root is skipped because it bypasses
// file permissions.
func makeUnreadable(t *testing.T, path string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions")
	}
	require.NoError(t, os.Chmod(path, 0o000))
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })
}
