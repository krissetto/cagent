package plan

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// lockFileName is the persistent sentinel file, inside the plans directory,
// that FilesystemStorage locks to serialize plan mutations across processes.
// It is intentionally never deleted: removing it would let two processes lock
// different inodes for the same logical directory and lose mutual exclusion.
// It can never be mistaken for a plan: List only considers .json files and no
// valid plan name can contain a dot.
const lockFileName = ".plans.lock"

// lockRetryInterval is how often a blocked acquireFileLock retries its
// non-blocking lock attempt while another process holds the lock.
const lockRetryInterval = 10 * time.Millisecond

// acquireFileLock takes an exclusive cross-process advisory lock on the
// sentinel file at path, creating the file (and any missing parent directory)
// first. It blocks until the lock is acquired or ctx is done. The returned
// release closure unlocks and closes the sentinel; defer it in the caller.
// Lock attempts are non-blocking with a retry loop because the platform lock
// primitives cannot be interrupted, so this is what honours cancellation
// while waiting.
func acquireFileLock(ctx context.Context, path string) (release func(), err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("creating plans directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening plans lock file %q: %w", path, err)
	}
	for {
		err := lockFileExclusive(f)
		if err == nil {
			// Release errors are ignored: closing the descriptor drops the
			// OS lock regardless, and there is no useful way to surface them.
			return func() {
				_ = unlockFile(f)
				_ = f.Close()
			}, nil
		}
		if !isLockUnavailable(err) {
			_ = f.Close()
			return nil, fmt.Errorf("locking plans lock file %q: %w", path, err)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(lockRetryInterval):
		}
	}
}
