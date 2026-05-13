package messages

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"gotest.tools/v3/assert"
)

func TestFindURLSpans(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		wantURLs []string
		wantCols [][2]int // [startCol, endCol] pairs
	}{
		{
			name:     "no URLs",
			text:     "hello world",
			wantURLs: nil,
		},
		{
			name:     "simple https URL",
			text:     "visit https://example.com for more",
			wantURLs: []string{"https://example.com"},
			wantCols: [][2]int{{6, 25}},
		},
		{
			name:     "http URL",
			text:     "go to http://example.com/path",
			wantURLs: []string{"http://example.com/path"},
			wantCols: [][2]int{{6, 29}},
		},
		{
			name:     "URL at start",
			text:     "https://example.com is a site",
			wantURLs: []string{"https://example.com"},
			wantCols: [][2]int{{0, 19}},
		},
		{
			name:     "URL at end",
			text:     "visit https://example.com",
			wantURLs: []string{"https://example.com"},
			wantCols: [][2]int{{6, 25}},
		},
		{
			name:     "URL with path and query",
			text:     "see https://example.com/path?q=1&b=2#frag for details",
			wantURLs: []string{"https://example.com/path?q=1&b=2#frag"},
			wantCols: [][2]int{{4, 41}},
		},
		{
			name:     "URL followed by period",
			text:     "Visit https://example.com.",
			wantURLs: []string{"https://example.com"},
			wantCols: [][2]int{{6, 25}},
		},
		{
			name:     "URL in parentheses",
			text:     "(https://example.com)",
			wantURLs: []string{"https://example.com"},
			wantCols: [][2]int{{1, 20}},
		},
		{
			name:     "URL with balanced parens in path",
			text:     "see https://en.wikipedia.org/wiki/Go_(programming_language) for more",
			wantURLs: []string{"https://en.wikipedia.org/wiki/Go_(programming_language)"},
			wantCols: [][2]int{{4, 59}},
		},
		{
			name:     "multiple URLs",
			text:     "see https://a.com and https://b.com for info",
			wantURLs: []string{"https://a.com", "https://b.com"},
			wantCols: [][2]int{{4, 17}, {22, 35}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := findURLSpans(tt.text)
			assert.Equal(t, len(tt.wantURLs), len(got), "span count mismatch")
			for i, span := range got {
				assert.Equal(t, tt.wantURLs[i], span.url, "url mismatch at index %d", i)
				assert.Equal(t, tt.wantCols[i][0], span.startCol, "startCol mismatch at index %d", i)
				assert.Equal(t, tt.wantCols[i][1], span.endCol, "endCol mismatch at index %d", i)
			}
		})
	}
}

func TestURLAtPosition(t *testing.T) {
	tests := []struct {
		name     string
		line     string
		col      int
		expected string
	}{
		{
			name:     "click on URL",
			line:     "visit https://example.com for more",
			col:      10,
			expected: "https://example.com",
		},
		{
			name:     "click before URL",
			line:     "visit https://example.com for more",
			col:      3,
			expected: "",
		},
		{
			name:     "click after URL",
			line:     "visit https://example.com for more",
			col:      28,
			expected: "",
		},
		{
			name:     "click on URL start",
			line:     "visit https://example.com for more",
			col:      6,
			expected: "https://example.com",
		},
		{
			name:     "click on URL last char",
			line:     "visit https://example.com for more",
			col:      24,
			expected: "https://example.com",
		},
		{
			name:     "line with ANSI codes",
			line:     "visit \x1b[34mhttps://example.com\x1b[0m for more",
			col:      10,
			expected: "https://example.com",
		},
		{
			name:     "empty line",
			line:     "",
			col:      0,
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := urlAtPosition(tt.line, tt.col)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestBalanceParens(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"https://example.com)", "https://example.com"},
		{"https://example.com/wiki/Go_(lang)", "https://example.com/wiki/Go_(lang)"},
		{"https://example.com", "https://example.com"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			assert.Equal(t, tt.expected, balanceParens(tt.input))
		})
	}
}

func TestUnderlineLine(t *testing.T) {
	tests := []struct {
		name     string
		line     string
		startCol int
		endCol   int
		wantSub  string // substring that should appear underlined
	}{
		{
			name:     "underlines URL portion",
			line:     "visit https://example.com for more",
			startCol: 6,
			endCol:   25,
			wantSub:  "https://example.com",
		},
		{
			name:     "preserves text before and after",
			line:     "before https://x.com after",
			startCol: 7,
			endCol:   19,
			wantSub:  "https://x.com",
		},
		{
			name:     "no-op when startCol >= endCol",
			line:     "hello world",
			startCol: 5,
			endCol:   5,
			wantSub:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := styleLineSegment(tt.line, tt.startCol, tt.endCol, underlineStyle)
			if tt.wantSub != "" {
				// The underlined text should contain the ANSI underline escape
				assert.Assert(t, strings.Contains(result, "\x1b["), "expected ANSI escape in result: %q", result)
				// The plain text of the result should still contain the URL
				plain := ansi.Strip(result)
				assert.Assert(t, strings.Contains(plain, tt.wantSub), "expected %q in plain text: %q", tt.wantSub, plain)
			} else {
				// No change expected
				assert.Equal(t, tt.line, result)
			}
		})
	}
}
