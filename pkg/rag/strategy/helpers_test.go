package strategy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
)

// newDBConfig creates a RAGDatabaseConfig for testing via YAML unmarshaling.
func newDBConfig(t *testing.T, value string) latest.RAGDatabaseConfig {
	t.Helper()
	var cfg latest.RAGDatabaseConfig
	err := cfg.UnmarshalYAML(func(v any) error {
		p, ok := v.(*string)
		if !ok {
			return nil
		}
		*p = value
		return nil
	})
	require.NoError(t, err)
	return cfg
}

func TestMakeAbsolute_WithParentDir(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	absolute := filepath.Join(t.TempDir(), "absolute", "file.go")
	assert.Equal(t, filepath.Join(parent, "relative.go"), makeAbsolute("relative.go", parent))
	assert.Equal(t, absolute, makeAbsolute(absolute, parent))
}

func TestMakeAbsolute_EmptyParentDir(t *testing.T) {
	t.Parallel()
	cwd, err := os.Getwd()
	require.NoError(t, err)

	result := makeAbsolute("relative.go", "")
	assert.Equal(t, filepath.Join(cwd, "relative.go"), result)
}

func TestResolveDatabasePath_EmptyParentDir(t *testing.T) {
	t.Parallel()
	cwd, err := os.Getwd()
	require.NoError(t, err)

	result, err := ResolveDatabasePath(newDBConfig(t, "./my.db"), "", "default")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(cwd, "my.db"), result)
}

func TestResolveDatabasePath_AbsolutePathIgnoresParentDir(t *testing.T) {
	t.Parallel()
	absolute := filepath.Join(t.TempDir(), "absolute", "my.db")
	result, err := ResolveDatabasePath(newDBConfig(t, absolute), t.TempDir(), "default")
	require.NoError(t, err)
	assert.Equal(t, absolute, result)
}

func TestResolveDatabasePath_RelativeWithParentDir(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	result, err := ResolveDatabasePath(newDBConfig(t, "./my.db"), parent, "default")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(parent, "my.db"), result)
}

func TestMergeDocPaths_EmptyParentDir(t *testing.T) {
	t.Parallel()
	cwd, err := os.Getwd()
	require.NoError(t, err)

	result := MergeDocPaths([]string{"shared.go"}, []string{"extra.go"}, "")
	assert.Equal(t, []string{
		filepath.Join(cwd, "shared.go"),
		filepath.Join(cwd, "extra.go"),
	}, result)
}
