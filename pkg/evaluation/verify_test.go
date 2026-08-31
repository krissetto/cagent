package evaluation

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunVerifyScript_EmptyScriptPasses(t *testing.T) {
	t.Parallel()
	result, err := runVerifyScript(t.Context(), "", "docker", "c")
	require.NoError(t, err)
	assert.True(t, result.Passed)
}

func TestRunVerifyScript_ZeroExitCodePasses(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell required")
	}
	t.Parallel()

	// Use a fake "container runtime" script that execs the verify command
	// directly (no real container needed).
	fake := writeFakeExecRuntime(t, "echo 'all good'; exit 0")

	result, err := runVerifyScript(t.Context(), "verify", fake, "container-1")
	require.NoError(t, err)
	assert.True(t, result.Passed)
	assert.Equal(t, 0, result.ExitCode)
	assert.Contains(t, result.Output, "all good")
}

func TestRunVerifyScript_NonZeroExitCodeFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell required")
	}
	t.Parallel()

	fake := writeFakeExecRuntime(t, "echo 'file missing'; exit 1")

	result, err := runVerifyScript(t.Context(), "verify", fake, "container-1")
	require.NoError(t, err)
	assert.False(t, result.Passed)
	assert.Equal(t, 1, result.ExitCode)
	assert.Contains(t, result.Output, "file missing")
}

func TestRunVerifyScript_OutputCapped(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell required")
	}
	t.Parallel()

	// Generate output larger than the cap.
	fake := writeFakeExecRuntime(t, "dd if=/dev/zero bs=1 count=20000 2>/dev/null | tr '\\0' 'x'; exit 0")

	result, err := runVerifyScript(t.Context(), "verify", fake, "container-1")
	require.NoError(t, err)
	assert.True(t, result.Passed)
	assert.LessOrEqual(t, len(result.Output), maxVerifyOutputBytes+20) // +20 for truncation marker
}

// writeFakeExecRuntime creates a shell script that ignores docker exec args
// and runs the given shell command instead, returning the script path.
func writeFakeExecRuntime(t *testing.T, shCmd string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-runtime")
	// The script ignores its arguments (exec container sh -c ...) and runs
	// shCmd directly, simulating what the container would do.
	script := "#!/bin/sh\n" + shCmd + "\n"
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	return path
}
