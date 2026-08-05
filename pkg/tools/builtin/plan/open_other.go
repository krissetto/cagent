//go:build !unix && !windows

package plan

import "os"

// OpenContentFile opens a plan-content source file read-only so the caller
// can validate what it actually opened — with File.Stat on the returned
// descriptor — before reading. Hanging opens are a Unix FIFO/device semantic
// defused there with O_NONBLOCK, and Windows needs an explicit
// FILE_SHARE_DELETE share mode so atomic replaces keep working while the
// descriptor is held; the remaining platforms (js/wasm, wasip1, plan9) have
// neither concern, so a plain open is used. Non-regular files are still
// rejected by the caller's descriptor check before any read.
func OpenContentFile(path string) (*os.File, error) {
	return os.Open(path)
}
