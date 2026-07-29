//go:build windows

package plan

import (
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

// OpenContentFile opens a plan-content source file read-only so the caller
// can validate what it actually opened — with File.Stat on the returned
// descriptor — before reading. Windows has no Unix FIFOs and no O_NONBLOCK,
// so hanging opens are not the concern here; sharing is. os.Open's share
// mode omits FILE_SHARE_DELETE, so a held descriptor would make the atomic
// rename that publishes a new plan revision (MoveFileEx with
// MOVEFILE_REPLACE_EXISTING) fail with "Access is denied" under concurrent
// reads. Opening with all three share flags lets a writer replace the path
// while this descriptor stays open and keeps reading the complete old
// contents. FILE_FLAG_BACKUP_SEMANTICS mirrors os.Open so a directory still
// opens and is rejected by the caller's descriptor check, like devices
// (e.g. NUL). The path is run through fixLongPath first because calling
// CreateFile directly skips os.Open's long-path handling; errors are
// wrapped in an os.PathError carrying the path as the caller spelled it.
func OpenContentFile(path string) (*os.File, error) {
	fixed, err := fixLongPath(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	pathp, err := windows.UTF16PtrFromString(fixed)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := windows.CreateFile(
		pathp,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(h), path)
	if f == nil {
		// Unreachable after a successful CreateFile; close rather than leak.
		_ = windows.CloseHandle(h)
		return nil, &os.PathError{Op: "open", Path: path, Err: windows.ERROR_INVALID_HANDLE}
	}
	return f, nil
}

// fixLongPath makes path safe to hand straight to CreateFile, which imposes
// the legacy 260-character MAX_PATH limit on ordinary Win32 paths — a limit
// os.Open lifts internally with its own fixLongPath. Ordinary paths are
// resolved with FullPath and re-rooted under the extended-length prefix:
// `\\?\` for drive paths, `\\?\UNC\` for UNC paths. Resolving first
// matters because the prefix disables Win32 path normalization, so
// working-directory resolution, "."/".." collapsing and slash conversion
// must all happen before it goes on. Unlike the stdlib, every ordinary path
// is converted instead of only those near the limit; the meaning is the
// same and it keeps this helper small. Device and already-extended paths
// (`\\.\`, `\\?\`, `\??\`) pass through untouched, as re-rooting them
// would change their meaning. The empty path is rejected with ENOENT up
// front, exactly as os.Open rejects it.
func fixLongPath(path string) (string, error) {
	if path == "" {
		return "", syscall.ENOENT
	}
	if isExtendedOrDevicePath(path) {
		return path, nil
	}
	abs, err := windows.FullPath(path)
	if err != nil {
		return "", err
	}
	// DOS device names resolve to device paths wherever they appear
	// (FullPath("NUL") is `\\.\NUL`), so the result needs the same
	// pass-through check as the input.
	if isExtendedOrDevicePath(abs) {
		return abs, nil
	}
	if len(abs) >= 2 && os.IsPathSeparator(abs[0]) && os.IsPathSeparator(abs[1]) {
		// \\server\share\... becomes \\?\UNC\server\share\...
		return `\\?\UNC` + abs[1:], nil
	}
	return `\\?\` + abs, nil
}

// isExtendedOrDevicePath reports whether path is a device path (`\\.\`) or
// an extended-length path (`\\?\`), with any separator mix as Win32
// accepts, or an NT object path (`\??\`, backslashes only) — the forms
// fixLongPath must leave alone. Mirrors the stdlib's addExtendedPrefix
// detection.
func isExtendedOrDevicePath(path string) bool {
	if len(path) < 4 {
		return false
	}
	if path[:4] == `\??\` {
		return true
	}
	return os.IsPathSeparator(path[0]) && os.IsPathSeparator(path[1]) &&
		(path[2] == '?' || path[2] == '.') && os.IsPathSeparator(path[3])
}
