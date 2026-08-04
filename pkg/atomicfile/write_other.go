//go:build !windows

package atomicfile

import (
	"io"
	"os"

	"github.com/natefinch/atomic"
)

// write publishes r at path with [atomic.WriteFile]'s temp-file + rename,
// then applies mode, which the wrapped call does not let the caller set.
func write(path string, r io.Reader, mode os.FileMode) error {
	if err := atomic.WriteFile(path, r); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}
