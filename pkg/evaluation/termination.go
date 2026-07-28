package evaluation

import (
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
)

// Bounds for text copied out of raw container events into termination
// markers and the associated assistant stop message. Runes rather than
// bytes so multi-byte content is not cut mid-character; a rune bound also
// bounds bytes at four per rune.
const (
	maxTerminationFieldRunes   = 256
	maxTerminationMessageRunes = 4096
)

// sanitizeEventText returns a bounded, control-free copy of a string taken
// from a raw container event. The policy is deliberately simple and
// deterministic: invalid UTF-8 sequences are dropped, control characters
// other than newline and tab are removed, surrounding whitespace is
// trimmed, and the result is capped at maxRunes runes.
func sanitizeEventText(s string, maxRunes int) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	count := 0
	for i := range s {
		if count == maxRunes {
			return strings.TrimRightFunc(s[:i], unicode.IsSpace)
		}
		count++
	}
	return s
}

// terminationFromEvent builds the allow-listed termination for a raw
// budget_exceeded container event. Only the known optional fields (budget,
// limit, used, max, config_path, message) are copied; a field that is
// absent, not a string, or empty after sanitization is omitted. Session,
// agent, and unknown fields are never copied.
func terminationFromEvent(event map[string]any) *session.Termination {
	field := func(key string, maxRunes int) string {
		v, ok := event[key].(string)
		if !ok {
			return ""
		}
		return sanitizeEventText(v, maxRunes)
	}
	return &session.Termination{
		Reason:     session.TerminationReasonBudgetExceeded,
		Budget:     field("budget", maxTerminationFieldRunes),
		Limit:      field("limit", maxTerminationFieldRunes),
		Used:       field("used", maxTerminationFieldRunes),
		Max:        field("max", maxTerminationFieldRunes),
		ConfigPath: field("config_path", maxTerminationFieldRunes),
		Message:    field("message", maxTerminationMessageRunes),
	}
}

// budgetStopMessageFromEvent extracts a safe copy of the assistant stop
// message embedded in a budget_exceeded event under "stop_message": agent
// name, role, content, and creation timestamp only, so hidden payload
// fields (tool calls, reasoning, model, usage) are never imported. Returns
// nil when the payload is missing, malformed, not an assistant message, or
// empty after sanitization; the marker then stands alone.
func budgetStopMessageFromEvent(event map[string]any) *session.Message {
	payload, ok := event["stop_message"].(map[string]any)
	if !ok {
		return nil
	}
	inner, ok := payload["message"].(map[string]any)
	if !ok {
		return nil
	}
	if role, _ := inner["role"].(string); role != string(chat.MessageRoleAssistant) {
		return nil
	}
	content, _ := inner["content"].(string)
	content = sanitizeEventText(content, maxTerminationMessageRunes)
	if content == "" {
		return nil
	}
	agentName, _ := payload["agent_name"].(string)
	createdAt, _ := inner["created_at"].(string)
	if _, err := time.Parse(time.RFC3339, createdAt); err != nil {
		createdAt = parseEventTimestamp(event)
	}
	return &session.Message{
		AgentName: sanitizeEventText(agentName, maxTerminationFieldRunes),
		Message: chat.Message{
			Role:      chat.MessageRoleAssistant,
			Content:   content,
			CreatedAt: createdAt,
		},
	}
}

// terminationTracker deduplicates the budget stops observed while a raw
// container event stream is replayed. SessionFromEvents and buildTranscript
// each feed events through their own tracker so both outputs agree on
// which events produce a marker.
//
// The budget_exceeded event is self-contained: it embeds the assistant
// stop message under stop_message, so no association with later events is
// needed. Every message_added event is ignored by the pipeline; it has no
// JSON payload, and regular assistant turns are already rebuilt from
// agent_choice events.
type terminationTracker struct {
	seen map[session.Termination]bool
}

// observeBudgetExceeded normalizes a budget_exceeded event into its two
// chronological outputs: the termination marker and, when the event embeds
// a valid stop_message, the assistant stop message the runtime recorded
// right after it. Both are nil when the same marker was already seen
// (repeated delivery or repeated processing must not duplicate it).
func (t *terminationTracker) observeBudgetExceeded(event map[string]any) (*session.Termination, *session.Message) {
	term := terminationFromEvent(event)
	if t.seen == nil {
		t.seen = make(map[session.Termination]bool)
	}
	if t.seen[*term] {
		return nil, nil
	}
	t.seen[*term] = true
	return term, budgetStopMessageFromEvent(event)
}

// terminationTranscriptMarker renders a termination as a transcript line,
// including only the optional details the event carried.
func terminationTranscriptMarker(t *session.Termination) string {
	var b strings.Builder
	b.WriteString("[Run terminated: ")
	b.WriteString(t.Reason)
	if t.ConfigPath != "" {
		b.WriteString(" at ")
		b.WriteString(t.ConfigPath)
	}
	if t.Used != "" && t.Max != "" {
		fmt.Fprintf(&b, " (used %s of %s)", t.Used, t.Max)
	}
	b.WriteString("]")
	return b.String()
}
