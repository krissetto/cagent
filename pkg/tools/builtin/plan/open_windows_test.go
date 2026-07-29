//go:build windows

package plan

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/atomicfile"
)

// TestOpenContentFile_AllowsReplaceWhileOpen proves the reader's half of
// the lock-free contract — the open shares deletion: the atomic rename
// that publishes a new plan revision (the same atomicfile.Write the
// storage uses, os.Root.Rename's POSIX-semantics rename underneath) must
// succeed while a reader still holds a descriptor on the destination, and
// that descriptor must keep reading the complete old contents. os.Open's
// share mode omits FILE_SHARE_DELETE, so a descriptor opened that way
// would make even that POSIX rename fail with a sharing violation under
// concurrent lock-free reads.
func TestOpenContentFile_AllowsReplaceWhileOpen(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "p.json")
	require.NoError(t, os.WriteFile(path, []byte("old"), 0o600))

	f, err := OpenContentFile(path)
	require.NoError(t, err)
	defer f.Close()

	require.NoError(t, atomicfile.Write(path, strings.NewReader("new"), 0o600),
		"replacing the path must not be blocked by the held read descriptor")

	held, err := io.ReadAll(f)
	require.NoError(t, err, "the held descriptor must stay readable after the replace")
	assert.Equal(t, "old", string(held), "the held descriptor must read the pre-replace contents")

	current, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "new", string(current))
}

// TestOpenContentFile_MissingFileIsNotExist pins the error mapping: callers
// distinguish a missing plan from a real failure with errors.Is(err,
// os.ErrNotExist), so the raw CreateFile error must arrive wrapped the way
// os.Open would wrap it.
func TestOpenContentFile_MissingFileIsNotExist(t *testing.T) {
	t.Parallel()

	f, err := OpenContentFile(filepath.Join(t.TempDir(), "absent.json"))
	require.ErrorIs(t, err, os.ErrNotExist)
	assert.Nil(t, f)
}

// TestOpenContentFile_LongPath proves the direct CreateFile call does not
// reintroduce the legacy MAX_PATH limit that os.Open's fixLongPath lifts: a
// file whose absolute path exceeds 260 characters must open and read.
// Depth comes from repeated moderate components so no single component
// approaches the 255-character per-component limit.
func TestOpenContentFile_LongPath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	for len(dir) <= 260 {
		dir = filepath.Join(dir, strings.Repeat("d", 64))
	}
	require.NoError(t, os.MkdirAll(dir, 0o700))
	path := filepath.Join(dir, "p.json")
	require.Greater(t, len(path), 260)
	require.NoError(t, os.WriteFile(path, []byte("deep"), 0o600))

	f, err := OpenContentFile(path)
	require.NoError(t, err, "a path beyond MAX_PATH must still open")
	defer f.Close()

	got, err := io.ReadAll(f)
	require.NoError(t, err)
	assert.Equal(t, "deep", string(got))
}

// TestOpenContentFile_EmptyPathIsNotExist pins os.Open's empty-path
// contract, which the empty string would otherwise smuggle past CreateFile
// as the current directory after path normalization.
func TestOpenContentFile_EmptyPathIsNotExist(t *testing.T) {
	t.Parallel()

	f, err := OpenContentFile("")
	require.ErrorIs(t, err, os.ErrNotExist)
	assert.Nil(t, f)

	var pathErr *os.PathError
	require.ErrorAs(t, err, &pathErr)
	assert.Equal(t, "open", pathErr.Op)
	assert.Empty(t, pathErr.Path)
}

// TestOpenContentFile_ErrorKeepsUserPath pins that failures report the path
// exactly as the caller spelled it, not the normalized extended-length form
// handed to CreateFile.
func TestOpenContentFile_ErrorKeepsUserPath(t *testing.T) {
	t.Parallel()

	userPath := t.TempDir() + "/./absent.json"
	_, err := OpenContentFile(userPath)

	var pathErr *os.PathError
	require.ErrorAs(t, err, &pathErr)
	assert.Equal(t, userPath, pathErr.Path)
}

// TestFixLongPath pins the normalization table: ordinary paths become
// absolute extended-length paths (with FullPath resolving slashes and dot
// segments first, since the `\\?\` prefix turns Win32 normalization off),
// while device, extended-length and NT object paths pass through untouched.
// None of the paths need to exist — the mapping is lexical.
func TestFixLongPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"drive absolute", `C:\plans\p.json`, `\\?\C:\plans\p.json`},
		{"drive with slashes and dot segments", `C:/plans/./deep/../p.json`, `\\?\C:\plans\p.json`},
		{"unc", `\\srv\share\p.json`, `\\?\UNC\srv\share\p.json`},
		{"unc with slashes", `//srv/share/p.json`, `\\?\UNC\srv\share\p.json`},
		{"already extended drive", `\\?\C:\plans\p.json`, `\\?\C:\plans\p.json`},
		{"already extended unc", `\\?\UNC\srv\share\p.json`, `\\?\UNC\srv\share\p.json`},
		{"extended with mixed separators", `//?/C:/plans/p.json`, `//?/C:/plans/p.json`},
		{"nt object path", `\??\C:\plans\p.json`, `\??\C:\plans\p.json`},
		{"device", `\\.\NUL`, `\\.\NUL`},
		{"device with slashes", `//./NUL`, `//./NUL`},
		{"bare device name", `NUL`, `\\.\NUL`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := fixLongPath(tc.in)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestFixLongPath_RelativeResolvesAgainstWorkingDirectory covers the
// resolution FullPath must perform before the extended prefix goes on,
// against the live working directory rather than a fixture.
func TestFixLongPath_RelativeResolvesAgainstWorkingDirectory(t *testing.T) {
	t.Parallel()

	wd, err := os.Getwd()
	require.NoError(t, err)
	if strings.HasPrefix(wd, `\\`) {
		t.Skipf("working directory %q is a UNC path; the expected prefix would differ", wd)
	}

	got, err := fixLongPath(`.\deep\..\p.json`)
	require.NoError(t, err)
	assert.Equal(t, `\\?\`+filepath.Join(wd, "p.json"), got)
}
