package promptfiles

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/fsx"
)

// MaxIndexEntries caps how many nested prompt files [Index] lists, so a
// monorepo with hundreds of sub-projects can't flood the context.
const MaxIndexEntries = 100

// Index renders a listing of the prompt files named by filenames that live
// below workDir — the monorepo case, where each sub-project carries its own
// AGENTS.md that the upward [Paths] lookup never sees.
//
// Only paths are listed, never contents: the cost stays proportional to the
// number of sub-projects instead of their size, and the agent reads the
// files it actually needs with its own tools. Paths already resolved by
// [Paths] are passed in loaded and skipped, so a file is never both injected
// and listed.
//
// maxDepth counts directory levels below workDir (1 = direct subdirectories,
// so only "<child>/AGENTS.md" is listed). Returns "" when maxDepth <= 0 (the
// feature is opt-in) or nothing is found.
func Index(workDir string, filenames []string, maxDepth int, loaded []string) string {
	root, err := filepath.Abs(workDir)
	if err != nil {
		return ""
	}
	found, truncated := nested(root, filenames, maxDepth, loaded)
	if len(found) == 0 {
		return ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Prompt files found in subdirectories of %s. Their contents are NOT loaded:\n\n", root)
	for _, path := range found {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			rel = path
		}
		fmt.Fprintf(&b, "- %s\n", filepath.ToSlash(rel))
	}
	if truncated {
		fmt.Fprintf(&b, "\n(only the first %d are listed)\n", MaxIndexEntries)
	}
	b.WriteString("\nRead the ones covering the files you are about to work on. For anything under its own directory, a nested file's instructions take precedence over the instructions loaded above.")
	return b.String()
}

// nested walks up to maxDepth levels below root and returns the paths of
// every file named after one of filenames, excluding loaded. Hidden and
// git-ignored directories are skipped. truncated reports that the walk hit
// [MaxIndexEntries] and stopped early; the returned prefix is still the
// lexically first entries, since [filepath.WalkDir] visits in order.
func nested(root string, filenames []string, maxDepth int, loaded []string) (paths []string, truncated bool) {
	if maxDepth <= 0 || len(filenames) == 0 {
		return nil, false
	}
	// Best-effort .gitignore filtering: nil (a no-op matcher) when root
	// isn't a git worktree root, leaving only the hidden-dir skip.
	ignore, _ := fsx.NewVCSMatcher(root)

	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		// Levels between root and the entry's parent directory: 0 for an
		// entry sitting directly in root.
		depth := strings.Count(filepath.ToSlash(rel), "/")

		if d.IsDir() {
			// Files inside this directory sit at depth+1, so entering it is
			// only useful while depth+1 <= maxDepth.
			if strings.HasPrefix(d.Name(), ".") || depth >= maxDepth || ignore.ShouldIgnore(path) {
				return fs.SkipDir
			}
			return nil
		}

		if depth == 0 || !slices.Contains(filenames, d.Name()) || slices.Contains(loaded, path) || ignore.ShouldIgnore(path) {
			return nil
		}
		if len(paths) == MaxIndexEntries {
			truncated = true
			return fs.SkipAll
		}
		paths = append(paths, path)
		return nil
	})
	return paths, truncated
}
