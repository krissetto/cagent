//go:build windows

package atomicfile_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"

	"github.com/docker/docker-agent/pkg/atomicfile"
)

// openShareDelete opens path read-only with all three share flags — the
// share mode plan.OpenContentFile uses for lock-free plan reads — without
// importing that package, which sits above atomicfile in the dependency
// graph. Sharing deletion is what entitles a writer to replace the path
// while the descriptor stays open.
func openShareDelete(t *testing.T, path string) *os.File {
	t.Helper()

	pathp, err := windows.UTF16PtrFromString(path)
	require.NoError(t, err)
	h, err := windows.CreateFile(
		pathp,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	require.NoError(t, err)
	f := os.NewFile(uintptr(h), path)
	require.NotNil(t, f)
	return f
}

// TestWriteCreatesFile covers first publication, which the mode-asserting
// tests skip on Windows: the rename has no destination to replace.
func TestWriteCreatesFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "f")
	require.NoError(t, atomicfile.Write(path, strings.NewReader("hello"), 0o600))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "hello", string(data))
}

// TestWriteNearComponentLimitBasename pins the temp naming: the sibling
// temp file uses a short generic pattern instead of the destination
// basename plus a suffix, so a valid basename near the 255-character
// component limit must not fail by growing past it.
func TestWriteNearComponentLimitBasename(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), strings.Repeat("n", 240))
	require.NoError(t, atomicfile.Write(path, strings.NewReader("hello"), 0o600))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "hello", string(data))
}

// TestWriteReplacesExistingFile covers replacement with no reader holding
// the destination, and pins that a successful publish leaves no temp file.
func TestWriteReplacesExistingFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	require.NoError(t, os.WriteFile(path, []byte("old"), 0o600))

	require.NoError(t, atomicfile.Write(path, strings.NewReader("new"), 0o600))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "new", string(data))

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "a successful write must leave only the destination behind")
}

// TestWriteReplacesWhileDestinationHeldOpen is the regression for the
// Windows CI failure: MoveFileEx with MOVEFILE_REPLACE_EXISTING fails with
// "Access is denied" while a reader holds the destination open even with
// FILE_SHARE_DELETE, whereas os.Root.Rename's POSIX-semantics rename must
// replace it, with the held descriptor still reading the old contents.
func TestWriteReplacesWhileDestinationHeldOpen(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "f")
	require.NoError(t, os.WriteFile(path, []byte("old"), 0o600))

	held := openShareDelete(t, path)
	defer held.Close()

	require.NoError(t, atomicfile.Write(path, strings.NewReader("new"), 0o600),
		"replacing the path must not be blocked by the held read descriptor")

	old, err := io.ReadAll(held)
	require.NoError(t, err, "the held descriptor must stay readable after the replace")
	assert.Equal(t, "old", string(old), "the held descriptor must read the pre-replace contents")

	current, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "new", string(current))
}

// symlinkOrSkip creates a symlink, skipping the test when the environment
// forbids it: without SeCreateSymbolicLinkPrivilege (granted to elevated
// processes or by Developer Mode), Windows refuses symlink creation with
// ERROR_PRIVILEGE_NOT_HELD.
func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()

	err := os.Symlink(target, link)
	if errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD) {
		t.Skipf("creating symlinks requires SeCreateSymbolicLinkPrivilege or Developer Mode: %v", err)
	}
	require.NoError(t, err)
}

// TestWriteReplacesSymlinkItselfNotTarget pins the security contract the
// plan storage relies on: an existing symlink at the destination is
// replaced rather than followed, so the rename must swap the link entry
// itself for a regular file and never write through it to the target.
func TestWriteReplacesSymlinkItselfNotTarget(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	require.NoError(t, os.WriteFile(target, []byte("precious"), 0o600))
	link := filepath.Join(dir, "link")
	symlinkOrSkip(t, target, link)

	require.NoError(t, atomicfile.Write(link, strings.NewReader("new"), 0o600))

	info, err := os.Lstat(link)
	require.NoError(t, err)
	assert.True(t, info.Mode().IsRegular(), "the write must replace the link entry itself, not follow it")

	data, err := os.ReadFile(link)
	require.NoError(t, err)
	assert.Equal(t, "new", string(data))

	data, err = os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "precious", string(data), "the symlink target must stay byte-identical")
}

// TestWriteReplacesDanglingSymlink covers the destination entry a stat
// pre-check cannot see: a dangling link must still be replaced by a
// regular file with the new contents.
func TestWriteReplacesDanglingSymlink(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	link := filepath.Join(dir, "link")
	symlinkOrSkip(t, filepath.Join(dir, "absent"), link)

	require.NoError(t, atomicfile.Write(link, strings.NewReader("new"), 0o600))

	info, err := os.Lstat(link)
	require.NoError(t, err)
	assert.True(t, info.Mode().IsRegular(), "the write must replace the dangling link entry")

	data, err := os.ReadFile(link)
	require.NoError(t, err)
	assert.Equal(t, "new", string(data))
}

// TestWriteFailureKeepsDestinationAndRemovesTemp pins the failure contract:
// a reader error must leave the previous contents untouched and must not
// leak the temp file next to them.
func TestWriteFailureKeepsDestinationAndRemovesTemp(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	require.NoError(t, os.WriteFile(path, []byte("old"), 0o600))

	err := atomicfile.Write(path, iotest.ErrReader(errors.New("boom")), 0o600)
	require.Error(t, err)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "old", string(data), "a failed write must leave the destination unmodified")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "a failed write must remove its temp file")
}
