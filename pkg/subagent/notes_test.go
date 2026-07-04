package subagent

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWrapAndDetectSystemInfo(t *testing.T) {
	t.Parallel()

	wrapped := WrapSystemInfo(`Subagent "worker" (a1b2c) finished.`)
	assert.True(t, IsSystemInfo(wrapped))
	assert.True(t, IsSystemInfo("  \n"+wrapped), "leading whitespace tolerated")
	assert.False(t, IsSystemInfo("just a normal user message"))
	assert.False(t, IsSystemInfo("mentions <system_info> mid-sentence"))
}

func TestMentionedSubagent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		content  string
		wantName string
		wantID   NodeID
		wantOK   bool
	}{
		{
			name:     "turn report",
			content:  WrapSystemInfo(`Subagent "worker" (a1b2c) finished.`),
			wantName: "worker", wantID: "a1b2c", wantOK: true,
		},
		{
			name:     "failure report",
			content:  WrapSystemInfo(`Subagent "coder" (0f9e8) failed.`),
			wantName: "coder", wantID: "0f9e8", wantOK: true,
		},
		{
			name:     "message from subagent",
			content:  WrapSystemInfo("Message from subagent \"planner\" (12ab3):\n\nhalfway there"),
			wantName: "planner", wantID: "12ab3", wantOK: true,
		},
		{
			name:     "read_subagent header",
			content:  `Subagent "worker" (a1b2c) — running`,
			wantName: "worker", wantID: "a1b2c", wantOK: true,
		},
		{
			name:    "no attribution",
			content: WrapSystemInfo("something else entirely"),
			wantOK:  false,
		},
		{
			name:    "legacy message without id",
			content: WrapSystemInfo(`Message from subagent "planner":` + "\n\nhello"),
			wantOK:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			name, id, ok := MentionedSubagent(tt.content)
			require.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.wantName, name)
			assert.Equal(t, tt.wantID, id)
		})
	}
}

func TestPreviewText(t *testing.T) {
	t.Parallel()

	preview, truncated := PreviewText("short answer", 50)
	assert.Equal(t, "short answer", preview)
	assert.False(t, truncated)

	preview, truncated = PreviewText("  spaced\n\nout\ttext  ", 50)
	assert.Equal(t, "spaced out text", preview)
	assert.False(t, truncated)

	long := strings.Repeat("abcde ", 20)
	preview, truncated = PreviewText(long, 50)
	assert.True(t, truncated)
	assert.LessOrEqual(t, len([]rune(preview)), 50)
	assert.NotEmpty(t, preview)

	// Rune-safe: multibyte content is never split mid-rune.
	preview, truncated = PreviewText(strings.Repeat("héllø ", 20), 50)
	assert.True(t, truncated)
	assert.True(t, utf8.ValidString(preview))

	preview, truncated = PreviewText("", 50)
	assert.Empty(t, preview)
	assert.False(t, truncated)
}
