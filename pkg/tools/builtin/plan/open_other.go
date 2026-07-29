//go:build !unix

package plan

import "os"

// OpenContentFile opens a plan-content source file read-only so the caller
// can validate what it actually opened — with File.Stat on the returned
// descriptor — before reading. Hanging opens are a Unix FIFO/device semantic
// defused there with O_NONBLOCK; the remaining platforms (Windows, js) have
// no O_NONBLOCK and no Unix FIFOs, and opening a filesystem path does not
// block the same way, so a plain open is used. Non-regular files (e.g.
// Windows device names like NUL) are still rejected by the caller's
// descriptor check before any read.
func OpenContentFile(path string) (*os.File, error) {
	return os.Open(path)
}
