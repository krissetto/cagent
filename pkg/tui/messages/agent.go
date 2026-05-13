package messages

// Agent messages control agent switching, commands, and model selection.
type (
	// SwitchAgentMsg switches to a different agent.
	SwitchAgentMsg struct{ AgentName string }

	// AgentCommandMsg sends a command to the agent.
	AgentCommandMsg struct{ Command string }

	// OpenModelPickerMsg opens the model picker dialog.
	OpenModelPickerMsg struct{}

	// ChangeModelMsg changes the model for the current agent.
	ChangeModelMsg struct{ ModelRef string }

	// OpenSubAgentTabMsg asks the TUI to register and switch to an attached
	// live session tab for the given subagent. SessionID is the subagent's
	// full child-session id (not the 5-char short ref).
	//
	// Emitted in response to a single click on a subagent row in the sidebar.
	// The TUI looks up the live node via the runtime's SessionTreeProvider,
	// attaches an event subscription via LiveEventSource, and surfaces the
	// session as a supervisor-owned attached tab.
	OpenSubAgentTabMsg struct{ SessionID string }

	// OpenSubAgentByShortRefMsg asks the chat page to resolve a 5-character
	// subagent ref visible in a tool-call row to the live child session's
	// full session id, then open or switch to that attached tab.
	//
	// Emitted in response to clicking a compact subagent tool-call row in the
	// transcript. The extra resolution step is needed because those rows only
	// carry the short user-facing ref, not the full session id.
	OpenSubAgentByShortRefMsg struct{ ShortRef string }

	// OpenParentSessionMsg asks the TUI to jump to the parent of an attached
	// sub-session tab. SessionID is the parent's session id. The TUI tries,
	// in order: switch to an existing open tab, then attach a live subagent
	// tab for the parent, then fall back to a friendly info notice.
	OpenParentSessionMsg struct{ SessionID string }
)
