package toolcommon

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// urlRuneSpan is a URL detected in a line, with its rune-index range.
type urlRuneSpan struct {
	url   string
	start int
	end   int // exclusive
}

// findURLRuneSpans finds http(s):// URLs in a line and returns their rune
// ranges. Matching mirrors the plain-text URL detection used for click
// handling (pkg/tui/components/messages/urldetect.go), so emitted OSC 8
// destinations agree with what detection finds on an unwrapped line.
func findURLRuneSpans(runes []rune) []urlRuneSpan {
	var spans []urlRuneSpan
	n := len(runes)

	for i := 0; i < n; {
		if runes[i] != 'h' {
			i++
			continue
		}
		remaining := string(runes[i:])
		var prefixLen int
		switch {
		case strings.HasPrefix(remaining, "https://"):
			prefixLen = len("https://")
		case strings.HasPrefix(remaining, "http://"):
			prefixLen = len("http://")
		default:
			i++
			continue
		}

		// Must not be preceded by a word character (avoid matching mid-word)
		if i > 0 && isURLWordRune(runes[i-1]) {
			i++
			continue
		}

		j := i + prefixLen
		for j < n && isURLRune(runes[j]) {
			j++
		}
		// Strip common trailing punctuation that's unlikely part of the URL
		for j > i+prefixLen && isTrailingURLPunct(runes[j-1]) {
			j--
		}
		// Strip a trailing ')' only if unmatched, e.g. "(https://example.com)"
		if hasUnbalancedCloseParen(runes[i:j]) {
			j--
		}

		spans = append(spans, urlRuneSpan{url: string(runes[i:j]), start: i, end: j})
		i = j
	}
	return spans
}

// writeChunkWithHyperlinks writes runes[start:end] to b, wrapping every part
// that overlaps a URL span in an OSC 8 hyperlink to the span's full URL.
// Wrapping can split a URL across lines; linking each visible fragment keeps
// the complete destination clickable everywhere.
func writeChunkWithHyperlinks(b *strings.Builder, runes []rune, start, end int, spans []urlRuneSpan) {
	pos := start
	for _, span := range spans {
		if span.end <= pos || span.start >= end {
			continue
		}
		from := max(span.start, pos)
		to := min(span.end, end)
		b.WriteString(string(runes[pos:from]))
		b.WriteString(ansi.SetHyperlink(span.url))
		b.WriteString(string(runes[from:to]))
		b.WriteString(ansi.ResetHyperlink())
		pos = to
	}
	b.WriteString(string(runes[pos:end]))
}

func hasUnbalancedCloseParen(url []rune) bool {
	if len(url) == 0 || url[len(url)-1] != ')' {
		return false
	}
	open, closed := 0, 0
	for _, r := range url {
		switch r {
		case '(':
			open++
		case ')':
			closed++
		}
	}
	return closed > open
}

func isURLRune(r rune) bool {
	if r <= ' ' || r == '"' || r == '<' || r == '>' || r == '{' || r == '}' || r == '|' || r == '\\' || r == '^' || r == '`' {
		return false
	}
	return true
}

func isURLWordRune(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

func isTrailingURLPunct(r rune) bool {
	return r == '.' || r == ',' || r == ';' || r == ':' || r == '!' || r == '?'
}
