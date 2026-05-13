package sidebar

import (
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// parentSessionLine renders the "parent: <agent>" line shown inside the
// Session tab of attached sub-session tabs. The line doubles as a clickable
// back-link: the sidebar's hit testing maps a click on this row to a
// [ClickParent] result carrying the parent's session id so the TUI can
// switch tabs.
//
// Owned tabs (no parent) get an empty string so no vertical space is paid.
//
// The `↩ parent` affordance is only drawn while the mouse is hovering the
// row; without hover we show the plain `parent: <agent>` label so the
// sidebar stays calm.
func (m *model) parentSessionLine(contentWidth int) string {
	if m.sessionState == nil || !m.sessionState.IsSubSession() {
		return ""
	}
	parentID := m.sessionState.ParentSessionID()
	if parentID == "" {
		return ""
	}

	name := strings.TrimSpace(m.sessionState.ParentAgentName())
	if name == "" {
		name = "parent"
	}

	nameStyle := styles.AgentAccentStyleFor(name).Bold(true)
	if m.hoveredParentLine {
		nameStyle = styles.Hovered(nameStyle)
	}
	label := styles.MutedStyle.Render("parent: ") + nameStyle.Render(name)
	if !m.hoveredParentLine {
		return label
	}
	hint := styles.MutedStyle.Render("↩ parent")
	avail := contentWidth - lipgloss.Width(label) - 1
	if avail <= 0 {
		return toolcommon.TruncateText(label, contentWidth)
	}
	if avail < lipgloss.Width(hint) {
		return label
	}
	padding := avail - lipgloss.Width(hint)
	return label + strings.Repeat(" ", padding) + " " + hint
}

// parentSessionLineCollapsed renders the parent-session indicator for the
// collapsed sidebar. Uses a single line so the collapsed layout math stays
// predictable.
func (m *model) parentSessionLineCollapsed() string {
	if m.sessionState == nil || !m.sessionState.IsSubSession() {
		return ""
	}
	parentID := m.sessionState.ParentSessionID()
	if parentID == "" {
		return ""
	}

	name := strings.TrimSpace(m.sessionState.ParentAgentName())
	if name == "" {
		name = "parent"
	}

	nameStyle := styles.AgentAccentStyleFor(name).Bold(true)
	if m.hoveredParentLine {
		nameStyle = styles.Hovered(nameStyle)
	}
	return styles.MutedStyle.Render("parent: ") + nameStyle.Render(name)
}
