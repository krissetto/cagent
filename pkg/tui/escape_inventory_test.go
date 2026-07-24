package tui

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNoVisibleEscapeCancellationGuidance(t *testing.T) {
	pattern := regexp.MustCompile(`(?i)["` + "`" + `][^"` + "`" + `]*(esc|escape)[^"` + "`" + `]*(cancel|close|interrupt|dismiss)[^"` + "`" + `]*["` + "`" + `]|["` + "`" + `][^"` + "`" + `]*(cancel|close|interrupt|dismiss)[^"` + "`" + `]*(esc|escape)[^"` + "`" + `]*["` + "`" + `]`)
	err := filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || filepath.Ext(path) != ".go" || len(path) >= 8 && path[len(path)-8:] == "_test.go" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, line := range regexp.MustCompile(`\r?\n`).Split(string(data), -1) {
			trimmed := regexp.MustCompile(`//.*$`).ReplaceAllString(line, "")
			require.NotRegexp(t, pattern, trimmed, "visible Escape cancellation guidance in %s: %s", path, line)
		}
		return nil
	})
	require.NoError(t, err)
}
