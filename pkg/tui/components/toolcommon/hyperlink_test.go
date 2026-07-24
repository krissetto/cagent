package toolcommon

import (
	"regexp"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// archiveURL reproduces issue #3821: a fetch tool argument long enough to be
// wrapped over several lines by wrapTextWithIndent.
const archiveURL = "https://web.archive.org/web/2024/https://www.gnu.org/software/coreutils/manual/html_node/Directories-and-the-Set_002dUser_002dID-and-Set_002dGroup_002dID-Bits.html"

var osc8LinkRe = regexp.MustCompile("\x1b\\]8;;([^\x07]*)\x07(.*?)\x1b\\]8;;\x07")

type renderedLink struct {
	url  string
	text string
}

func osc8Links(line string) []renderedLink {
	var links []renderedLink
	for _, m := range osc8LinkRe.FindAllStringSubmatch(line, -1) {
		links = append(links, renderedLink{url: m[1], text: m[2]})
	}
	return links
}

func TestFindURLRuneSpans(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		text      string
		wantURLs  []string
		wantSpans [][2]int // [start, end) rune ranges
	}{
		{
			name:     "no URLs",
			text:     "hello world",
			wantURLs: nil,
		},
		{
			name:      "fetch args with more suffix",
			text:      "(" + archiveURL + " (+1 more))",
			wantURLs:  []string{archiveURL},
			wantSpans: [][2]int{{1, 1 + len(archiveURL)}},
		},
		{
			name:      "single URL in parentheses",
			text:      "(https://example.com)",
			wantURLs:  []string{"https://example.com"},
			wantSpans: [][2]int{{1, 20}},
		},
		{
			name:      "URL with balanced parens in path",
			text:      "(https://en.wikipedia.org/wiki/Go_(programming_language))",
			wantURLs:  []string{"https://en.wikipedia.org/wiki/Go_(programming_language)"},
			wantSpans: [][2]int{{1, 56}},
		},
		{
			name:      "trailing punctuation stripped",
			text:      "see https://example.com.",
			wantURLs:  []string{"https://example.com"},
			wantSpans: [][2]int{{4, 23}},
		},
		{
			name:      "close paren then colon",
			text:      "(https://example.com): Calling https://example.com",
			wantURLs:  []string{"https://example.com", "https://example.com"},
			wantSpans: [][2]int{{1, 20}, {31, 50}},
		},
		{
			name:     "not preceded by word character",
			text:     "xhttps://example.com",
			wantURLs: nil,
		},
		{
			name:      "multiple URLs",
			text:      "https://a.com https://b.com",
			wantURLs:  []string{"https://a.com", "https://b.com"},
			wantSpans: [][2]int{{0, 13}, {14, 27}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := findURLRuneSpans([]rune(tt.text))
			require.Len(t, got, len(tt.wantURLs))
			for i, span := range got {
				assert.Equal(t, tt.wantURLs[i], span.url)
				assert.Equal(t, tt.wantSpans[i][0], span.start)
				assert.Equal(t, tt.wantSpans[i][1], span.end)
			}
		})
	}
}

func TestWrapTextWithIndentHyperlinksWrappedURL(t *testing.T) {
	t.Parallel()

	got := wrapTextWithIndent("(https://ex.com/abcdef)", 10, 12)

	want := "(\x1b]8;;https://ex.com/abcdef\x07https://e\x1b]8;;\x07\n" +
		"  \x1b]8;;https://ex.com/abcdef\x07x.com/abcdef\x1b]8;;\x07\n" +
		"  )"
	assert.Equal(t, want, got)
}

// TestWrapTextWithIndentWrappedFetchArgs is a regression test for issue #3821:
// the fetch/api tool renders "(<url> (+1 more))" and wrapTextWithIndent splits
// the URL over multiple lines. Every visible fragment must carry the full URL
// as an OSC 8 hyperlink, while the surrounding parens and "(+1 more)" suffix
// stay outside the link. The visible text and wrapping must be unchanged.
func TestWrapTextWithIndentWrappedFetchArgs(t *testing.T) {
	t.Parallel()

	text := "(" + archiveURL + " (+1 more))"
	const firstLineWidth, subsequentLineWidth = 66, 78

	got := wrapTextWithIndent(text, firstLineWidth, subsequentLineWidth)
	lines := strings.Split(got, "\n")
	require.Greater(t, len(lines), 1, "URL should wrap over multiple lines")

	var linked strings.Builder
	for i, line := range lines {
		width := firstLineWidth
		if i > 0 {
			width = subsequentLineWidth + 2 // subsequent lines carry the 2-cell indent
		}
		assert.LessOrEqual(t, lipgloss.Width(line), width)

		for _, link := range osc8Links(line) {
			assert.Equal(t, archiveURL, link.url, "every fragment must link to the full URL")
			linked.WriteString(link.text)
		}
	}

	// Fragments cover exactly the URL: nothing missing, parens and the
	// "(+1 more)" suffix excluded from the hyperlink.
	assert.Equal(t, archiveURL, linked.String())

	// Visible rendering is unchanged: stripping OSC 8 and the "\n  " wraps
	// reconstructs the original text.
	assert.Equal(t, text, strings.ReplaceAll(ansi.Strip(got), "\n  ", ""))
}

func TestWrapTextWithIndentNoWrapNoHyperlinks(t *testing.T) {
	t.Parallel()

	t.Run("single line that fits is unchanged", func(t *testing.T) {
		t.Parallel()
		text := "(https://example.com)"
		assert.Equal(t, text, wrapTextWithIndent(text, 40, 40))
	})

	t.Run("multi-line input with fitting lines is unchanged except indent", func(t *testing.T) {
		t.Parallel()
		got := wrapTextWithIndent("https://a.com\nhttps://b.com", 40, 40)
		assert.Equal(t, "https://a.com\n  https://b.com", got)
	})
}
