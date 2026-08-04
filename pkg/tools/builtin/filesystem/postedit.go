//go:build !js

package filesystem

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"github.com/docker/docker-agent/pkg/shellpath"
)

// runPostEditCommands executes configured shell commands after a file edit.
func runPostEditCommands(ctx context.Context, workingDir string, postEditCommands []PostEditConfig, filePath string) error {
	for _, postEdit := range postEditCommands {
		if !matchPostEdit(ctx, postEdit.Path, workingDir, filePath) {
			continue
		}

		shell, argsPrefix := shellpath.DetectShell()
		cmd := exec.CommandContext(ctx, shell, append(argsPrefix, postEdit.Cmd)...)
		cmd.Env = cmd.Environ()
		cmd.Env = append(cmd.Env, "file="+filePath)

		if err := cmd.Run(); err != nil {
			return fmt.Errorf("post-edit command failed for %s: %w", filePath, err)
		}
	}
	return nil
}

func matchPostEdit(ctx context.Context, patternStr, workingDir, filePath string) bool {
	pattern := filepath.ToSlash(patternStr)
	target := filepath.Base(filePath)
	if strings.Contains(pattern, "/") {
		if workingDir != "" {
			rel, err := filepath.Rel(workingDir, filePath)
			if err == nil {
				target = filepath.ToSlash(rel)
			} else {
				slog.DebugContext(ctx, "Failed to resolve relative path for post-edit pattern", "workingDir", workingDir, "filePath", filePath, "error", err)
				target = filepath.ToSlash(filePath)
			}
		} else {
			target = filepath.ToSlash(filePath)
		}
	}
	matched, err := path.Match(pattern, target)
	if err != nil {
		slog.WarnContext(ctx, "Invalid post-edit pattern", "pattern", patternStr, "error", err)
		return false
	}
	return matched
}
