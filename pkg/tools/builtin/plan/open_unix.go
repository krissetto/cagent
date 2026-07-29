//go:build unix

package plan

import (
	"os"
	"syscall"
)

// OpenContentFile opens a plan-content source file read-only so the caller
// can validate what it actually opened — with File.Stat on the returned
// descriptor — before reading, instead of trusting a pre-open stat of the
// path that a concurrent swap could invalidate. The open itself must not
// hang: a plain open(2) of a FIFO with no writer blocks forever, and some
// devices (e.g. serial lines) block on open too, so the file is opened with
// O_NONBLOCK, which makes those opens return immediately. Callers reject
// anything non-regular after inspecting the descriptor; for the regular
// files that remain, O_NONBLOCK has no effect on reads, so the flag is left
// set. It is exported so host-side callers (e.g. the docker agent plans CLI)
// open user-supplied content paths with the same hang-safety instead of
// duplicating the platform logic.
func OpenContentFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
