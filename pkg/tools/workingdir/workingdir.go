package workingdir

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/docker/docker-agent/pkg/path"
)

// Resolve returns the effective working directory for a toolset process.
func Resolve(toolsetWorkingDir, agentWorkingDir string) string {
	if toolsetWorkingDir == "" {
		return agentWorkingDir
	}
	toolsetWorkingDir = path.ExpandPath(toolsetWorkingDir)
	if filepath.IsAbs(toolsetWorkingDir) {
		return filepath.Clean(toolsetWorkingDir)
	}
	if agentWorkingDir != "" {
		abs, err := filepath.Abs(filepath.Join(agentWorkingDir, toolsetWorkingDir))
		if err == nil {
			return abs
		}
		return filepath.Join(agentWorkingDir, toolsetWorkingDir)
	}
	return toolsetWorkingDir
}

// CheckDirExists returns an error if the given directory does not exist or is
// not a directory. kind is used only in the error message.
func CheckDirExists(dir, kind string) error {
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("working_dir %q for %s toolset does not exist", dir, kind)
		}
		return fmt.Errorf("working_dir %q for %s toolset: %w", dir, kind, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("working_dir %q for %s toolset is not a directory", dir, kind)
	}
	return nil
}
