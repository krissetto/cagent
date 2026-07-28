//go:build js && wasm

package plan

import "os"

// lockFileExclusive is a no-op under js/wasm: the runtime is single-threaded
// and there is no second process to coordinate with, so the in-process mutex
// in FilesystemStorage is sufficient.
func lockFileExclusive(_ *os.File) error {
	return nil
}

func unlockFile(_ *os.File) error {
	return nil
}

func isLockUnavailable(error) bool {
	return false
}
