package listdirectory

import (
	"testing"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/filesystem"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestExtractResult(t *testing.T) {
	tests := []struct {
		name     string
		meta     *filesystem.ListDirectoryMeta
		expected string
	}{
		{
			name:     "nil meta",
			meta:     nil,
			expected: "empty directory",
		},
		{
			name:     "empty directory",
			meta:     &filesystem.ListDirectoryMeta{},
			expected: "empty directory",
		},
		{
			name:     "only files",
			meta:     &filesystem.ListDirectoryMeta{Files: []string{"a", "b", "c"}},
			expected: "3 files",
		},
		{
			name:     "only one file",
			meta:     &filesystem.ListDirectoryMeta{Files: []string{"a"}},
			expected: "1 file",
		},
		{
			name:     "only directories",
			meta:     &filesystem.ListDirectoryMeta{Dirs: []string{"a", "b"}},
			expected: "2 directories",
		},
		{
			name:     "only one directory",
			meta:     &filesystem.ListDirectoryMeta{Dirs: []string{"a"}},
			expected: "1 directory",
		},
		{
			name:     "mixed files and directories",
			meta:     &filesystem.ListDirectoryMeta{Files: []string{"a", "b", "c"}, Dirs: []string{"d", "e"}},
			expected: "3 files and 2 directories",
		},
		{
			name:     "truncated output",
			meta:     &filesystem.ListDirectoryMeta{Files: []string{"a", "b"}, Dirs: []string{"c"}, Truncated: true},
			expected: "2 files and 1 directory (truncated)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := &types.Message{}
			if tt.meta != nil {
				msg.ToolResult = &tools.ToolCallResult{Meta: *tt.meta}
			}
			result := extractResult(msg)
			if result != tt.expected {
				t.Errorf("extractResult() = %q, want %q", result, tt.expected)
			}
		})
	}
}

func TestFormatCount(t *testing.T) {
	tests := []struct {
		count    int
		singular string
		plural   string
		expected string
	}{
		{0, "file", "files", "0 files"},
		{1, "file", "files", "1 file"},
		{2, "file", "files", "2 files"},
		{100, "file", "files", "100 files"},
		{1, "directory", "directories", "1 directory"},
		{2, "directory", "directories", "2 directories"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			result := formatCount(tt.count, tt.singular, tt.plural)
			if result != tt.expected {
				t.Errorf("formatCount(%d, %q, %q) = %q, want %q",
					tt.count, tt.singular, tt.plural, result, tt.expected)
			}
		})
	}
}

func TestShortenPath(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		expected string
	}{
		{
			name:     "empty path",
			path:     "",
			expected: "",
		},
		{
			name:     "current directory",
			path:     ".",
			expected: ".",
		},
		{
			name:     "absolute path",
			path:     "/usr/local/bin",
			expected: "/usr/local/bin",
		},
		{
			name:     "relative path",
			path:     "src/components",
			expected: "src/components",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := toolcommon.ShortenPath(tt.path)
			if result != tt.expected {
				t.Errorf("ShortenPath(%q) = %q, want %q", tt.path, result, tt.expected)
			}
		})
	}
}
