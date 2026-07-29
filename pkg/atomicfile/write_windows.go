//go:build windows

package atomicfile

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// write publishes r at path via a fully written, synced sibling temp file
// renamed into place with [os.Root.Rename], whose Windows implementation
// renames with POSIX semantics (NtSetInformationFile with
// FILE_RENAME_INFORMATION_EX and FILE_RENAME_POSIX_SEMANTICS). Unlike
// natefinch/atomic's MoveFileEx with MOVEFILE_REPLACE_EXISTING — "Access
// is denied" while any descriptor is open on the destination — it replaces
// a destination whose readers share deletion. On filesystems lacking
// FILE_RENAME_INFORMATION_EX or POSIX semantics (e.g. FAT), Go 1.26's
// renameat retries by itself with an ordinary replacing rename, which
// preserves the legacy behavior when no reader holds the destination; no
// extra fallback belongs here. mode is ignored per the package contract,
// and the os APIs keep os.Open's long-path handling.
func write(path string, r io.Reader, _ os.FileMode) error {
	dir := filepath.Dir(path)
	// Generic temp name, not derived from the destination: appending a
	// suffix to a basename near the 255-character component limit would
	// make the temp name invalid.
	f, err := os.CreateTemp(dir, ".atomic-*")
	if err != nil {
		return fmt.Errorf("creating temp file in %q: %w", dir, err)
	}
	tmp := f.Name()
	// The rename consumes the temp name on success, so this only collects
	// the temp file after a failure.
	defer os.Remove(tmp)

	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return fmt.Errorf("writing temp file %q: %w", tmp, err)
	}
	// Sync before the rename so a crash cannot publish an empty or torn file.
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("flushing temp file %q: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing temp file %q: %w", tmp, err)
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("opening directory %q: %w", dir, err)
	}
	defer root.Close()
	if err := root.Rename(filepath.Base(tmp), filepath.Base(path)); err != nil {
		return fmt.Errorf("replacing %q with temp file %q: %w", path, tmp, err)
	}
	return nil
}
