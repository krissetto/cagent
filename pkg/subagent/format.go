package subagent

import "strings"

// TruncatePreview normalises whitespace and trims the text to at most
// limit runes. It returns the resulting preview and whether truncation
// occurred.
func TruncatePreview(s string, limit int) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	// Collapse noisy whitespace so previews are compact and predictable.
	s = strings.Join(strings.Fields(s), " ")
	if limit <= 0 {
		limit = DefaultPreviewLimit
	}
	runes := []rune(s)
	if len(runes) <= limit {
		return s, false
	}
	if limit <= 1 {
		return string(runes[:limit]), true
	}
	return string(runes[:limit-1]) + "…", true
}

// FormatEnvelopeMessage renders the runtime-generated message that the
// parent loop injects into the conversation when a subagent update is
// delivered.
func FormatEnvelopeMessage(env Envelope) string {
	var b strings.Builder
	b.WriteString("<subagent_update>\n")
	b.WriteString("subagent_id: ")
	b.WriteString(ShortRef(env.SubAgentID))
	b.WriteString("\nagent: ")
	b.WriteString(env.AgentName)
	b.WriteString("\nkind: ")
	b.WriteString(string(env.Kind))
	b.WriteString("\nstatus: ")
	b.WriteString(env.Status.String())
	if env.Preview != "" {
		b.WriteString("\npreview: ")
		b.WriteString(env.Preview)
		if env.Truncated {
			b.WriteString(" [truncated]")
		}
	}
	if env.Error != "" {
		b.WriteString("\nerror: ")
		b.WriteString(env.Error)
	}
	b.WriteString("\nUse subagent_send to follow up. Use subagent_inspect only if the preview above omits details you need.\n")
	b.WriteString("</subagent_update>")
	return b.String()
}
