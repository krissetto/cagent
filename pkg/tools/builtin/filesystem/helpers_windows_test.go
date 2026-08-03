//go:build windows

package filesystem

import (
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

// Post-edit commands run under PowerShell on Windows
// (shellpath.DetectShell). These helpers mirror the POSIX equivalents in
// helpers_other_test.go so the tests validate the same contracts on every
// platform.

// postEditMarkerCmd returns a command creating an empty "<path>.formatted"
// marker file, where <path> arrives in the "file" environment variable
// injected by runPostEditCommands. Explicit concatenation keeps the
// suffix out of the variable name.
func postEditMarkerCmd() string {
	return `New-Item -ItemType File -Force -Path ($env:file + '.formatted') | Out-Null`
}

// makeUnreadable revokes read access to path for the rest of the test.
// POSIX modes cannot express that on Windows (0o000 merely sets the
// read-only attribute), so the helper holds an exclusive handle — share
// mode 0 — until the test ends: any subsequent open fails with
// ERROR_SHARING_VIOLATION, while os.Stat keeps succeeding because
// metadata access is exempt from sharing checks.
func makeUnreadable(t *testing.T, path string) {
	t.Helper()
	p, err := windows.UTF16PtrFromString(path)
	require.NoError(t, err)
	h, err := windows.CreateFile(p, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = windows.CloseHandle(h) })
}
