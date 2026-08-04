//go:build !js

package filesystem

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMatchPostEdit(t *testing.T) {
	ctx := t.Context()
	workDir := filepath.Join(string(filepath.Separator), "workspace", "app")

	tests := []struct {
		name       string
		pattern    string
		workingDir string
		filePath   string
		wantMatch  bool
	}{
		{
			name:       "basename pattern matches simple file",
			pattern:    "*.go",
			workingDir: workDir,
			filePath:   filepath.Join(workDir, "main.go"),
			wantMatch:  true,
		},
		{
			name:       "basename pattern matches nested file",
			pattern:    "*.go",
			workingDir: workDir,
			filePath:   filepath.Join(workDir, "pkg", "sub", "foo.go"),
			wantMatch:  true,
		},
		{
			name:       "path-scoped pattern matches relative subpath",
			pattern:    "pkg/*.go",
			workingDir: workDir,
			filePath:   filepath.Join(workDir, "pkg", "foo.go"),
			wantMatch:  true,
		},
		{
			name:       "regression test: pkg/*.go does not match pkg/sub/file.go",
			pattern:    "pkg/*.go",
			workingDir: workDir,
			filePath:   filepath.Join(workDir, "pkg", "sub", "file.go"),
			wantMatch:  false,
		},
		{
			name:       "path-scoped pattern does not match different subpath",
			pattern:    "cmd/*.go",
			workingDir: workDir,
			filePath:   filepath.Join(workDir, "pkg", "foo.go"),
			wantMatch:  false,
		},
		{
			name:       "nested slash pattern matches multi-level path",
			pattern:    "pkg/sub/*.go",
			workingDir: workDir,
			filePath:   filepath.Join(workDir, "pkg", "sub", "bar.go"),
			wantMatch:  true,
		},
		{
			name:       "empty working dir falls back to slash-normalized file path",
			pattern:    "*.go",
			workingDir: "",
			filePath:   filepath.Join("pkg", "foo.go"),
			wantMatch:  true,
		},
		{
			name:       "invalid pattern returns false",
			pattern:    "[invalid",
			workingDir: workDir,
			filePath:   filepath.Join(workDir, "foo.go"),
			wantMatch:  false,
		},
		{
			name:       "file outside workingDir does not match path-scoped pattern",
			pattern:    "pkg/*.go",
			workingDir: workDir,
			filePath:   filepath.Join(workDir, "..", "outside", "foo.go"),
			wantMatch:  false,
		},
		{
			name:       "relative workingDir vs absolute filePath debug log fallback does not match relative pattern",
			pattern:    "pkg/*.go",
			workingDir: "relative/dir",
			filePath:   filepath.Join(workDir, "pkg", "foo.go"),
			wantMatch:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchPostEdit(ctx, tt.pattern, tt.workingDir, tt.filePath)
			assert.Equal(t, tt.wantMatch, got)
		})
	}
}
