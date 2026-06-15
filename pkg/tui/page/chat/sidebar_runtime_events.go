package chat

import (
	tea "charm.land/bubbletea/v2"
)

// ApplySidebarRuntimeEvent applies a runtime event to the sidebar only. The TUI
// uses this for root aggregate/tree events that should refresh the currently
// visible related tab even when the event is routed to another session's chat
// transcript.
func (p *chatPage) ApplySidebarRuntimeEvent(msg tea.Msg) tea.Cmd {
	switch msg.(type) {
	case nil:
		return nil
	default:
		return p.forwardToSidebar(msg)
	}
}
