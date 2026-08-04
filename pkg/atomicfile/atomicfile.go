// Package atomicfile writes files atomically (write-to-temp + rename)
// with a configurable file mode.
//
// On non-Windows platforms it wraps [github.com/natefinch/atomic.WriteFile],
// which performs the atomic rename but does not let the caller specify a
// permission bitmask on the resulting file. [Write] addresses that gap by
// chmod-ing the file after the rename, so the same call site can both
// publish the file atomically and ensure it is not world-readable.
//
// # Platform Support
//
// File modes are POSIX-only. On Windows, the mode parameter is ignored and
// the file inherits default ACLs from the parent directory. Windows also
// publishes through [os.Root.Rename] rather than natefinch/atomic: its
// POSIX-semantics rename replaces a destination that concurrent readers
// still hold open (provided they share deletion), where the MoveFileEx
// replace fails with "Access is denied".
//
// # Security Note
//
// On non-Windows platforms the chmod happens after the rename, creating a
// small window (typically microseconds) where the file exists at the default
// umask permissions before being tightened. Callers that require strict
// secrecy should ensure the parent directory has restrictive permissions
// (e.g., 0o700) to limit access during this window. On Windows there is no
// such window because the mode is ignored entirely.
package atomicfile

import (
	"io"
	"os"
)

// Write atomically writes data from r to path and sets the file's mode
// (POSIX only; see the package documentation for Windows).
//
// The write goes to a temporary file in the same directory and is then
// renamed into place; readers therefore observe either the previous
// contents or the new contents, never a partial write. On non-Windows
// platforms the chmod is applied after the rename: callers that care
// about secrecy should avoid having a third party already holding an
// open descriptor on path before the call.
func Write(path string, r io.Reader, mode os.FileMode) error {
	return write(path, r, mode)
}
