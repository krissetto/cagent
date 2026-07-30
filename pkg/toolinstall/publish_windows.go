//go:build windows

package toolinstall

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

func publishBinary(name, target string) error {
	binDir := BinDir()
	if err := os.MkdirAll(binDir, 0o755); err != nil { //nolint:gosec // executable search path must be traversable
		return fmt.Errorf("creating bin directory: %w", err)
	}

	link := filepath.Join(binDir, executableName(name))
	tmpLink := link + ".tmp." + strconv.Itoa(os.Getpid())
	_ = os.Remove(tmpLink)
	if err := os.Link(target, tmpLink); err != nil {
		return fmt.Errorf("creating temp hard link %s -> %s: %w", tmpLink, target, err)
	}
	_ = os.Remove(link)
	if err := os.Rename(tmpLink, link); err != nil {
		_ = os.Remove(tmpLink)
		return fmt.Errorf("renaming hard link %s -> %s: %w", tmpLink, link, err)
	}
	return nil
}
