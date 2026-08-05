//go:build !unix && !windows

package plan

import "os"

// The platforms without a usable cross-process lock primitive (js/wasm,
// wasip1, plan9) get no-op lock operations, so acquireFileLock degrades to
// just creating the sentinel file. Under js/wasm the runtime is
// single-threaded and there is no second process to coordinate with; on
// wasip1 and plan9 mutations are NOT serialized against other processes —
// only the in-process mutex of FilesystemStorage (s.mu) still guards
// concurrent mutations within this process.
func lockFileExclusive(_ *os.File) error {
	return nil
}

func unlockFile(_ *os.File) error {
	return nil
}

func isLockUnavailable(error) bool {
	return false
}
