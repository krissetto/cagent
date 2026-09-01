package promptfiles

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIndexDisabledByDefault(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writePrompt(t, makeDir(t, root, "child"), "child")

	assert.Empty(t, Index(root, []string{promptFile}, 0, nil))
}

func TestIndexListsPathsNotContents(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writePrompt(t, makeDir(t, root, "child"), "child content")

	note := Index(root, []string{promptFile}, 1, nil)

	assert.Contains(t, note, "- child/"+promptFile)
	assert.NotContains(t, note, "child content")
	assert.Contains(t, note, root)
}

func TestIndexHonoursDepth(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	child := makeDir(t, root, "child")
	grandChild := makeDir(t, child, "grandchild")
	writePrompt(t, child, "child")
	writePrompt(t, grandChild, "grandchild")

	depth1 := Index(root, []string{promptFile}, 1, nil)
	assert.Contains(t, depth1, "- child/"+promptFile)
	assert.NotContains(t, depth1, "grandchild")

	depth2 := Index(root, []string{promptFile}, 2, nil)
	assert.Contains(t, depth2, "- child/grandchild/"+promptFile)
}

func TestIndexSkipsLoadedAndRootFile(t *testing.T) {
	t.Parallel()

	// The workdir file is loaded in full by Paths; it must not also be
	// listed, and neither must a nested file the caller already loaded.
	root := t.TempDir()
	writePrompt(t, root, "root")
	child := makeDir(t, root, "child")
	loaded := writePrompt(t, child, "child")

	assert.Empty(t, Index(root, []string{promptFile}, 1, []string{loaded}))
}

func TestIndexSkipsHiddenDirs(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writePrompt(t, makeDir(t, root, ".cache"), "hidden")

	assert.Empty(t, Index(root, []string{promptFile}, 3, nil))
}

func TestIndexSkipsGitIgnoredDirs(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".gitignore"), []byte("node_modules/\n"), 0o600))
	writePrompt(t, makeDir(t, root, "node_modules"), "vendored")
	writePrompt(t, makeDir(t, root, "service"), "service")

	note := Index(root, []string{promptFile}, 2, nil)

	assert.Contains(t, note, "- service/"+promptFile)
	assert.NotContains(t, note, "node_modules")
}

func TestIndexMergesFilenames(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	child := makeDir(t, root, "child")
	writePrompt(t, child, "agents")
	require.NoError(t, os.WriteFile(filepath.Join(child, "OTHER.md"), []byte("other"), 0o600))

	note := Index(root, []string{promptFile, "OTHER.md"}, 1, nil)

	assert.Contains(t, note, "- child/"+promptFile)
	assert.Contains(t, note, "- child/OTHER.md")
}

func TestIndexCapsEntries(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	for i := range MaxIndexEntries + 5 {
		// Zero-padded so the lexical walk order matches the numeric one.
		writePrompt(t, makeDir(t, root, "p"+strconv.Itoa(1000+i)), "content")
	}

	note := Index(root, []string{promptFile}, 1, nil)

	assert.Equal(t, MaxIndexEntries, countEntries(note))
	assert.Contains(t, note, "only the first 100 are listed")
}

func countEntries(note string) int {
	count := 0
	for line := range strings.SplitSeq(note, "\n") {
		if strings.HasPrefix(line, "- ") {
			count++
		}
	}
	return count
}
