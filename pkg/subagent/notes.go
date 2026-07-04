package subagent

import (
	"regexp"
	"strings"
)

const (
	systemInfoOpen  = "<system_info>"
	systemInfoClose = "</system_info>"
)

// WrapSystemInfo wraps a note the runtime writes on an agent's behalf (a
// spawn task, turn report, or relayed message) so the model can distinguish
// harness/system information from ordinary conversation content.
func WrapSystemInfo(body string) string {
	return systemInfoOpen + "\n" + body + "\n" + systemInfoClose
}

// PreviewLen is the maximum length (in runes) of the response preview
// embedded in a turn report.
const PreviewLen = 50

// PreviewText condenses s into a single-line preview of at most limit runes,
// collapsing all whitespace runs to single spaces. truncated reports whether
// content was cut (the caller marks it, e.g. with a trailing "[...]").
func PreviewText(s string, limit int) (preview string, truncated bool) {
	s = strings.Join(strings.Fields(s), " ")
	runes := []rune(s)
	if len(runes) <= limit {
		return s, false
	}
	return strings.TrimSpace(string(runes[:limit])), true
}

// IsSystemInfo reports whether content is a runtime-authored system_info
// note (a subagent turn report or relayed message).
func IsSystemInfo(content string) bool {
	return strings.HasPrefix(strings.TrimSpace(content), systemInfoOpen)
}

// mentionRe matches the attribution the runtime stamps on subagent notes
// and tool results: `subagent "name" (id)` in any phrasing
// ("Subagent … finished", "Message from subagent …", "Spawned subagent …",
// "Message delivered to subagent …"). The id is a 5-char git-like short sha
// (see NewID).
var mentionRe = regexp.MustCompile(`(?i:subagent) "([^"]+)" \(([0-9a-f]{5})\)`)

// MentionedSubagent extracts the subagent display name and node id from a
// note or tool-result header. ok is false when content carries no
// attribution.
func MentionedSubagent(content string) (name string, id NodeID, ok bool) {
	m := mentionRe.FindStringSubmatch(content)
	if m == nil {
		return "", "", false
	}
	return m[1], NodeID(m[2]), true
}
