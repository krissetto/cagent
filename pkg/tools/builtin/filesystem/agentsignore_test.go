package filesystem

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/fsx"
)

func ignoreProject(t *testing.T, ignoreBody string, files map[string]string) (string, *ToolSet) {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, fsx.AgentsIgnoreFile), []byte(ignoreBody), 0o644))
	for name, content := range files {
		full := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
	}

	ts, err := CreateToolSet(latest.Toolset{Type: "filesystem"}, &config.RuntimeConfig{
		Config: config.Config{WorkingDir: dir},
	})
	require.NoError(t, err)
	fsts, ok := ts.(*ToolSet)
	require.True(t, ok)
	return dir, fsts
}

func TestAgentsIgnoreBlocksRead(t *testing.T) {
	dir, ts := ignoreProject(t, "secrets.env\n", map[string]string{
		"secrets.env": "API_KEY=super-secret",
		"README.md":   "hello",
	})

	_, err := ts.resolveAndCheckPath(filepath.Join(dir, "secrets.env"))
	require.Error(t, err, "an ignored file must not be readable")
	assert.Contains(t, err.Error(), fsx.AgentsIgnoreFile)
	assert.NotContains(t, err.Error(), "API_KEY", "the error must not leak contents")

	_, err = ts.resolveAndCheckPath(filepath.Join(dir, "README.md"))
	assert.NoError(t, err, "unignored files stay readable")
}

func TestAgentsIgnoreBlocksWrite(t *testing.T) {
	dir, ts := ignoreProject(t, "secrets.env\nbuild/\n", nil)

	for _, target := range []string{"secrets.env", "build/artifact.bin"} {
		_, err := ts.resolveAndCheckPath(filepath.Join(dir, target))
		assert.Errorf(t, err, "writing %s must be refused", target)
	}
}

func TestAgentsIgnoreBlocksCreationOfMissingPath(t *testing.T) {
	dir, ts := ignoreProject(t, "*.key\n", nil)

	_, err := ts.resolveAndCheckPath(filepath.Join(dir, "id_rsa.key"))
	require.Error(t, err, "creating a new ignored file must be refused")
}

func TestAgentsIgnoreHidesFromListing(t *testing.T) {
	dir, ts := ignoreProject(t, "secrets.env\n", map[string]string{
		"secrets.env": "k=v",
		"README.md":   "hello",
	})

	assert.True(t, ts.shouldIgnorePath(filepath.Join(dir, "secrets.env")))
	assert.False(t, ts.shouldIgnorePath(filepath.Join(dir, "README.md")))
}

func TestAgentsIgnoreAppliesWhenIgnoreVCSDisabled(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, fsx.AgentsIgnoreFile), []byte("secrets.env\n"), 0o644))

	off := false
	ts, err := CreateToolSet(latest.Toolset{Type: "filesystem", IgnoreVCS: &off}, &config.RuntimeConfig{
		Config: config.Config{WorkingDir: dir},
	})
	require.NoError(t, err)
	fsts := ts.(*ToolSet)

	assert.True(t, fsts.shouldIgnorePath(filepath.Join(dir, "secrets.env")),
		"ignore_vcs: false must not disable .agentsignore")
	_, err = fsts.resolveAndCheckPath(filepath.Join(dir, "secrets.env"))
	require.Error(t, err)
}

func TestAgentsIgnoreHidesItself(t *testing.T) {
	dir, ts := ignoreProject(t, "secrets.env\n", nil)

	_, err := ts.resolveAndCheckPath(filepath.Join(dir, fsx.AgentsIgnoreFile))
	require.Error(t, err, ".agentsignore must not be readable by the agent")
}

func TestNoAgentsIgnoreLeavesBehaviourUnchanged(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "secrets.env"), []byte("k=v"), 0o644))

	ts, err := CreateToolSet(latest.Toolset{Type: "filesystem"}, &config.RuntimeConfig{
		Config: config.Config{WorkingDir: dir},
	})
	require.NoError(t, err)
	fsts := ts.(*ToolSet)

	require.Nil(t, fsts.agentsIgnore)
	_, err = fsts.resolveAndCheckPath(filepath.Join(dir, "secrets.env"))
	assert.NoError(t, err, "without the file nothing is newly blocked")
}

func TestAgentsIgnoreBlocksRelativePath(t *testing.T) {
	dir, ts := ignoreProject(t, "secrets.env\n", map[string]string{"secrets.env": "k=v"})

	for _, form := range []string{"secrets.env", "./secrets.env", "sub/../secrets.env"} {
		_, err := ts.resolveAndCheckPath(filepath.Join(dir, form))
		assert.Errorf(t, err, "%q must be blocked regardless of spelling", form)
	}
}

func TestAgentsIgnoreNegationReIncludes(t *testing.T) {
	dir, ts := ignoreProject(t, "*.key\n!public.key\n", map[string]string{
		"private.key": "secret",
		"public.key":  "shareable",
	})

	_, err := ts.resolveAndCheckPath(filepath.Join(dir, "private.key"))
	require.Error(t, err)

	_, err = ts.resolveAndCheckPath(filepath.Join(dir, "public.key"))
	assert.NoError(t, err, "! negation must re-include, as in .gitignore")
}

func TestAgentsIgnoreUnreadableFileIsAnError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, fsx.AgentsIgnoreFile)
	require.NoError(t, os.WriteFile(path, []byte("secrets.env\n"), 0o644))
	makeUnreadable(t, path)

	_, err := CreateToolSet(latest.Toolset{Type: "filesystem"}, &config.RuntimeConfig{
		Config: config.Config{WorkingDir: dir},
	})
	require.Error(t, err, "an unreadable .agentsignore must fail loudly")
	assert.Contains(t, err.Error(), fsx.AgentsIgnoreFile)
}
