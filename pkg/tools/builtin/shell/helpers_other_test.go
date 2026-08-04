//go:build !windows

package shell

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The shell tool runs commands under $SHELL (or /bin/sh) on non-Windows
// platforms. These helpers generate the POSIX commands mirrored for
// PowerShell in helpers_windows_test.go so the tests validate the same
// contracts on every platform.

// assertPlatformDefaultShell asserts New detected the platform shell:
// wantUnixShell (derived from $SHELL by the caller) with the -c prefix.
func assertPlatformDefaultShell(t *testing.T, h *shellHandler, wantUnixShell string) {
	t.Helper()
	assert.Equal(t, wantUnixShell, h.shell)
	assert.Equal(t, []string{"-c"}, h.shellArgsPrefix)
}

// pwdCmd returns a command printing the working directory.
func pwdCmd() string {
	return "pwd"
}

// envDumpCmd returns a command printing the full environment of the
// spawned process as "key=value" lines.
func envDumpCmd() string {
	return "env"
}

func envDumpContainsName(output, name string) bool {
	return strings.Contains(output, "name="+name)
}

// printEnvValueCmd returns a command printing the value of one
// environment variable, without a trailing newline.
func printEnvValueCmd(name string) string {
	return `printf '%s' "$` + name + `"`
}

// printEnvValuesAndPwdCmd returns a command printing the values of
// envVars plus the working directory, joined with "|".
func printEnvValuesAndPwdCmd(envVars ...string) string {
	args := make([]string, 0, len(envVars)+1)
	for _, name := range envVars {
		args = append(args, `"$`+name+`"`)
	}
	args = append(args, `"$(pwd)"`)
	format := strings.TrimSuffix(strings.Repeat("%s|", len(args)), "|")
	return `printf '` + format + `' ` + strings.Join(args, " ")
}

// repeatMessageCmd returns a command printing the $message env var
// $count times, one per line.
func repeatMessageCmd() string {
	return "for i in $(seq 1 $count); do echo $message; done"
}

// assertSamePath asserts that want and got refer to the same directory.
// EvalSymlinks makes the comparison stable on macOS, where TempDir lives
// under /var -> /private/var.
func assertSamePath(t *testing.T, want, got string) {
	t.Helper()
	wantResolved, err := filepath.EvalSymlinks(want)
	require.NoError(t, err)
	gotResolved, err := filepath.EvalSymlinks(got)
	require.NoError(t, err)
	assert.Equal(t, wantResolved, gotResolved)
}
