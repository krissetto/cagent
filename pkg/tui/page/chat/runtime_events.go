package chat

import (
	"fmt"
	"log/slog"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/sound"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/components/sidebar"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/types"
	"github.com/docker/docker-agent/pkg/userconfig"
)

// Runtime Event Handling
//
// This file maps runtime events to UI updates, following the Elm Architecture
// pattern of explicit event-to-update mappings.
//
// # Subagent tree refresh model (single source of truth)
//
// The sidebar's subagent tree is sourced exclusively from
// [runtime.SessionTreeProvider.LiveSessionTree]. There is exactly one event
// that asks the chat page to re-read it: [runtime.LiveSessionTreeChangedEvent].
// Every other subagent-related event (SubAgentStarted, SubAgentSent,
// SubAgentUpdate) is treated as immediate-parent UI feedback only — they
// never re-seed the sidebar tree. This keeps the data flow simple:
//
//   - tree topology  → LiveSessionTreeChangedEvent → refreshSubagentTree
//   - row state      → SubAgent* events            → direct sidebar update
//   - turn lifecycle → transcript cards            → messages component
//
// Together with the runtime's manager hooks (which fan tree-change
// notifications to every ancestor session bus), this guarantees correct
// updates at any nesting depth without any redundant refresh paths.
//
// Events are organized by category:
//
// Stream Lifecycle (session-lifetime):
//   - StreamStartedEvent  → Mark stream depth, sidebar tab lifetime
//   - StreamStoppedEvent  → Final cleanup, process queue, maybe exit
//
// Turn Lifecycle (per model turn):
//   - TurnStartedEvent  → Start spinners, set pending response
//   - TurnEndedEvent    → Stop spinner for the current turn
//
// Content Events:
//   - AgentChoiceEvent         → Append text to message
//   - AgentChoiceReasoningEvent → Append reasoning block
//   - UserMessageEvent         → Replace loading with user message
//
// Tool Events:
//   - PartialToolCallEvent      → Show tool call in progress
//   - ToolCallEvent             → Tool execution started
//   - ToolCallConfirmationEvent → Show confirmation dialog
//   - ToolCallResponseEvent     → Show tool result
//
// Sidebar Updates (forwarded):
//   - TokenUsageEvent, AgentInfoEvent, TeamInfoEvent, etc.
//
// Dialogs:
//   - MaxIterationsReachedEvent → Show max iterations dialog
//   - ElicitationRequestEvent   → Show elicitation/OAuth dialog

// handleRuntimeEvent processes runtime events and returns the appropriate command.
// Returns (handled, cmd) where handled indicates if the event was processed.
//
// The switch is organized by event category for clarity.
func (p *chatPage) handleRuntimeEvent(msg tea.Msg) (bool, tea.Cmd) {
	switch msg := msg.(type) {
	// ===== Error and Warning Events =====
	case *runtime.ErrorEvent:
		if userconfig.Get().GetSound() {
			sound.Play(sound.Failure)
		}
		return true, p.messages.AddErrorMessage(msg.Error)

	case *runtime.WarningEvent:
		return true, notification.WarningCmd(msg.Message)

	case *runtime.ModelFallbackEvent:
		// Update sidebar with the fallback model immediately so it reflects the switch
		sidebarCmd := p.sidebar.SetAgentInfo(msg.AgentName, msg.FallbackModel, "")
		// Notify user when switching to a fallback model, include the reason
		fallbackMsg := fmt.Sprintf("Model %s failed (%s), switching to %s", msg.FailedModel, msg.Reason, msg.FallbackModel)
		return true, tea.Batch(sidebarCmd, notification.WarningCmd(fallbackMsg))

	// ===== Stream Lifecycle Events =====
	case *runtime.StreamStartedEvent:
		return true, p.handleStreamStarted(msg)

	case *runtime.StreamStoppedEvent:
		return true, p.handleStreamStopped(msg)

	case *runtime.TurnStartedEvent:
		return true, p.handleTurnStarted(msg)

	case *runtime.TurnEndedEvent:
		return true, p.handleTurnEnded(msg)

	case *runtime.ParentIdleEvent:
		return true, p.handleParentIdle(msg)

	case *runtime.ParentResumeEvent:
		return true, p.handleParentResume(msg)

	// ===== Content Events =====
	case *runtime.UserMessageEvent:
		// Subagent envelope reminders are represented in the TUI as dedicated
		// MessageTypeSubAgent cards driven by SubAgentUpdateEvent below.
		// Suppress the raw implicit user-message form so we don't render both.
		if msg.Kind == session.MessageKindSubagentEnvelope {
			return true, nil
		}
		replaceCmd := p.messages.ReplaceLoadingWithUser(msg.Message, msg.SessionPosition)
		// Attached subagent tabs: a UserMessageEvent on the child's bus means
		// the child loop just accepted a new turn (parent delegation,
		// subagent_send, or a user-typed send from this tab). Prime the
		// working indicator now instead of waiting for StreamStartedEvent,
		// which can be delayed by slow tool loading.
		if p.sessionState != nil && p.sessionState.IsSubSession() {
			return true, tea.Batch(p.setWorking(true), replaceCmd)
		}
		return true, replaceCmd

	case *runtime.AgentChoiceEvent:
		return true, p.handleAgentChoice(msg)

	case *runtime.AgentChoiceReasoningEvent:
		return true, p.handleAgentChoiceReasoning(msg)

	case *runtime.SubAgentStartedEvent:
		return true, p.handleSubAgentStarted(msg)

	case *runtime.SubAgentSentEvent:
		return true, p.handleSubAgentSent(msg)

	case *runtime.SubAgentUpdateEvent:
		return true, p.handleSubAgentUpdate(msg)

	case *runtime.LiveSessionTreeChangedEvent:
		// Tree topology changed somewhere in this session's subtree (typically a
		// nested grandchild was created or transitioned). Refresh the sidebar's
		// live-tree snapshot so the new descendants show up immediately, without
		// adding any transcript noise.
		p.refreshSubagentTree()
		return true, nil

	case *runtime.ShellOutputEvent:
		return true, p.messages.AddShellOutputMessage(msg.Output)

	// ===== Tool Events =====
	case *runtime.PartialToolCallEvent:
		return true, p.handlePartialToolCall(msg)

	case *runtime.ToolCallEvent:
		return true, p.handleToolCall(msg)

	case *runtime.ToolCallConfirmationEvent:
		return true, p.handleToolCallConfirmation(msg)

	case *runtime.ToolCallResponseEvent:
		return true, p.handleToolCallResponse(msg)

	// ===== Sidebar Info Events (forwarded) =====
	case *runtime.TokenUsageEvent:
		p.handleTokenUsage(msg)
		return true, nil

	case *runtime.AgentInfoEvent:
		sidebarCmd := p.sidebar.SetAgentInfo(msg.AgentName, msg.Model, msg.Description)
		p.messages.AddWelcomeMessage(msg.WelcomeMessage)
		return true, sidebarCmd

	case *runtime.TeamInfoEvent:
		p.sidebar.SetTeamInfo(msg.AvailableAgents)
		// Invalidate the messages render cache so that agent-colored pills
		// (subagent delegations, handoffs, etc.) pick up the freshly populated
		// palette. On session reload the first View() happens before this
		// event arrives, so stale fallback-color renders get cached.
		p.messages.InvalidateRenderCache()
		return true, nil

	case *runtime.AgentSwitchingEvent:
		p.sidebar.SetAgentSwitching(msg.Switching)
		return true, nil

	case *runtime.ToolsetInfoEvent:
		p.sidebar.SetSkillsInfo(len(p.app.CurrentAgentSkills()))
		return true, p.forwardToSidebar(msg)

	case *runtime.SessionTitleEvent:
		return true, p.forwardToSidebar(msg)

	case *runtime.SessionCompactionEvent:
		if msg.Status == "completed" {
			return true, tea.Batch(
				p.setWorking(false),
				p.setPendingResponse(false),
				notification.SuccessCmd("Session compacted successfully."),
				p.messages.ScrollToBottom(),
			)
		}
		return true, nil

	// ===== RAG Indexing Events (forwarded to sidebar) =====
	case *runtime.RAGIndexingStartedEvent,
		*runtime.RAGIndexingProgressEvent,
		*runtime.RAGIndexingCompletedEvent:
		return true, p.forwardToSidebar(msg)

	// ===== Dialog Events =====
	case *runtime.MaxIterationsReachedEvent:
		return true, p.handleMaxIterationsReached(msg)

	case *runtime.ElicitationRequestEvent:
		return true, p.handleElicitationRequest(msg)
	}

	return false, nil
}

// forwardToSidebar forwards a message to the sidebar and returns the resulting command.
func (p *chatPage) forwardToSidebar(msg tea.Msg) tea.Cmd {
	slog.Debug("Forwarding event to sidebar", "event_type", fmt.Sprintf("%T", msg))
	model, cmd := p.sidebar.Update(msg)
	p.sidebar = model.(sidebar.Model)
	return cmd
}

// handleTokenUsage updates sidebar and session with token usage data.
// This handler performs side effects only and returns no command.
func (p *chatPage) handleTokenUsage(msg *runtime.TokenUsageEvent) {
	p.sidebar.SetTokenUsage(msg)
	if msg.Usage != nil {
		if sess := p.app.Session(); sess != nil {
			// Only update the parent session's token counts when the event
			// belongs to this session. Sub-sessions emit their own
			// TokenUsageEvents with a different SessionID; writing those
			// values into the parent would overwrite the parent's own
			// context-tracking counters.
			if msg.SessionID == "" || msg.SessionID == sess.ID {
				sess.InputTokens = msg.Usage.InputTokens
				sess.OutputTokens = msg.Usage.OutputTokens
			}

			// Track per-message usage for /cost dialog
			if msg.Usage.LastMessage != nil {
				sess.AddMessageUsageRecord(
					msg.AgentName,
					msg.Usage.LastMessage.Model,
					msg.Usage.LastMessage.Cost,
					&msg.Usage.LastMessage.Usage,
				)
			}
		}
	}
}

func (p *chatPage) handleStreamStarted(msg *runtime.StreamStartedEvent) tea.Cmd {
	slog.Debug("handleStreamStarted called", "agent", msg.AgentName, "session_id", msg.SessionID)
	// Stream lifecycle is session-scoped: track nesting depth and stream
	// start time so the cancel/sound logic in handleStreamStopped works,
	// but leave per-turn working/pending transitions to TurnStarted/TurnEnded.
	p.streamCancelled = false
	p.streamDepth++
	p.parentIdleDepth = 0
	p.streamStartTime = time.Now()
	return p.forwardToSidebar(msg)
}

// handleTurnStarted owns the per-turn UI transitions: it lights the working
// spinner for every turn. The pending-response indicator is only shown before
// the first assistant content in this stream lifetime; later turns after tool
// calls are already covered by the bottom-right working spinner, and re-showing
// the pending placeholder there is visually noisy.
// StreamStarted no longer drives these because it now fires once per session
// lifetime rather than once per turn (see runtime TurnStartedEvent).
func (p *chatPage) handleTurnStarted(msg *runtime.TurnStartedEvent) tea.Cmd {
	slog.Debug("handleTurnStarted called", "agent", msg.AgentName, "session_id", msg.SessionID)
	// Starting a new turn ends any prior parent-idle scope on this stream.
	p.parentIdleDepth = 0
	spinnerCmd := p.setWorking(true)
	var pendingCmd tea.Cmd
	if !p.hasReceivedAssistantContent {
		pendingCmd = p.setPendingResponse(true)
	}
	sidebarCmd := p.forwardToSidebar(msg)
	return tea.Batch(pendingCmd, spinnerCmd, sidebarCmd)
}

// handleTurnEnded clears the working indicator for the current turn. It
// intentionally does *not* process the queued-message follow-up or trigger
// the exit-after-first-response path: those are session-lifetime concerns
// that StreamStoppedEvent still owns. Clearing pending-response here is
// safe — it's a no-op once any chunk has arrived.
func (p *chatPage) handleTurnEnded(msg *runtime.TurnEndedEvent) tea.Cmd {
	slog.Debug("handleTurnEnded called", "agent", msg.AgentName, "session_id", msg.SessionID)
	spinnerCmd := p.setWorking(false)
	pendingCmd := p.setPendingResponse(false)
	sidebarCmd := p.forwardToSidebar(msg)
	return tea.Batch(spinnerCmd, pendingCmd, sidebarCmd)
}

func (p *chatPage) handleAgentChoice(msg *runtime.AgentChoiceEvent) tea.Cmd {
	if p.streamCancelled {
		return nil
	}
	// Track that we've received assistant content
	p.hasReceivedAssistantContent = true
	// Clear pending response indicator - first chunk has arrived
	p.setPendingResponse(false)
	return p.messages.AppendToLastMessage(msg.AgentName, msg.Content)
}

func (p *chatPage) handleAgentChoiceReasoning(msg *runtime.AgentChoiceReasoningEvent) tea.Cmd {
	if p.streamCancelled {
		return nil
	}
	p.setPendingResponse(false)
	return p.messages.AppendReasoning(msg.AgentName, msg.Content)
}

func (p *chatPage) handleSubAgentStarted(msg *runtime.SubAgentStartedEvent) tea.Cmd {
	// The delegation tool-call line already fully conveys this action as
	// `[parent] → [child] · <id>`. Rendering a second transcript card here
	// duplicates the same information and was explicitly called out as noise.
	//
	// Tree re-seeding is driven exclusively by LiveSessionTreeChangedEvent;
	// the direct SubAgentStartedEvent is still forwarded so the immediate
	// parent's sidebar can update its local state cheaply.
	return p.forwardToSidebar(msg)
}

func (p *chatPage) handleSubAgentSent(msg *runtime.SubAgentSentEvent) tea.Cmd {
	// Same rationale as handleSubAgentStarted: the `subagent_send` tool-call
	// line already shows `[parent] → <id>`, so an extra transcript card adds
	// no new information.
	//
	// Tree re-seeding is driven exclusively by LiveSessionTreeChangedEvent;
	// the direct SubAgentSentEvent is still forwarded so the immediate
	// parent's sidebar can flip the child back to "working" immediately.
	return p.forwardToSidebar(msg)
}

func (p *chatPage) handleSubAgentUpdate(msg *runtime.SubAgentUpdateEvent) tea.Cmd {
	// Status-only updates (e.g. from MarkWaitingSilently after ESC in an
	// attached child tab) must refresh the sidebar row but must not add a
	// transcript card — there is no completed turn to report.
	if msg.Envelope.Kind == subagent.UpdateKindStatusOnly {
		return p.forwardToSidebar(msg)
	}

	kind := types.SubAgentEventTurnCompleted
	if msg.Envelope.Kind != "" {
		switch msg.Envelope.Kind {
		case "closed":
			kind = types.SubAgentEventClosed
		case "stopped":
			kind = types.SubAgentEventStopped
		case "failed":
			kind = types.SubAgentEventFailed
		default:
			kind = types.SubAgentEventTurnCompleted
		}
	}

	// Build the chat card. Turn-completed cards intentionally carry no
	// preview detail — the row only announces "turn finished" so the parent's
	// transcript stays terse and any concrete content is reserved for an
	// explicit subagent_inspect call. Failures still surface their error
	// detail because that text is actionable.
	detail := ""
	if kind == types.SubAgentEventFailed {
		if msg.Envelope.Error != "" {
			detail = msg.Envelope.Error
		} else {
			detail = msg.Envelope.Preview
		}
	}

	info := types.SubAgentInfo{
		Kind:      kind,
		AgentName: msg.Envelope.AgentName,
		ShortID:   subagent.ShortRef(msg.Envelope.SubAgentID),
		Detail:    detail,
		Truncated: kind == types.SubAgentEventFailed && msg.Envelope.Truncated,
	}
	return tea.Batch(
		p.forwardToSidebar(msg),
		p.messages.AddSubAgentMessage(info),
		p.messages.ScrollToBottom(),
	)
}

// refreshSubagentTree re-seeds the sidebar's subagent section from the
// runtime's current live tree. This picks up nested descendants (grandchildren
// etc.) that are not directly visible through the root session's event stream.
// It is intentionally idempotent and cheap when the tree hasn't changed.
func (p *chatPage) refreshSubagentTree() {
	if p.app == nil {
		return
	}
	rootID := p.liveTreeRootID()
	if rootID == "" {
		return
	}
	nodes := p.app.LiveSessionTree(rootID)
	if len(nodes) > 0 {
		p.sidebar.SeedSubagentsFromLiveTree(nodes)
	}
}

func (p *chatPage) handleParentIdle(msg *runtime.ParentIdleEvent) tea.Cmd {
	p.parentIdleDepth++
	return tea.Batch(
		p.setWorking(false),
		// Forward to the sidebar so it can mirror the "no active parent
		// work" state — specifically, stop spinning the parent agent row
		// in the Agents list. Subagent rows keep spinning on their own.
		p.forwardToSidebar(msg),
	)
}

func (p *chatPage) handleParentResume(msg *runtime.ParentResumeEvent) tea.Cmd {
	if p.parentIdleDepth > 0 {
		p.parentIdleDepth--
	}
	sidebarCmd := p.forwardToSidebar(msg)
	// Only resume the main spinner if the stream is still active. If the
	// outer stream has already ended, StreamStoppedEvent will own the final
	// working-state cleanup.
	if p.streamDepth > 0 {
		return tea.Batch(sidebarCmd, p.setWorking(true))
	}
	return sidebarCmd
}

func (p *chatPage) handleStreamStopped(msg *runtime.StreamStoppedEvent) tea.Cmd {
	slog.Debug("handleStreamStopped called",
		"agent", msg.AgentName,
		"session_id", msg.SessionID,
		"reason", msg.Reason,
		"should_exit", p.app.ShouldExitAfterFirstResponse(),
		"has_content", p.hasReceivedAssistantContent,
		"stream_depth", p.streamDepth)

	if p.streamDepth > 0 {
		p.streamDepth--
	}

	sidebarCmd := p.forwardToSidebar(msg)

	// Sub-agent stream stopped — the parent is still running, so only
	// forward to the sidebar and keep the working/cancel state intact.
	// Without this guard, pressing Esc after a sub-agent completes but
	// while the parent continues would have no effect.
	if p.streamDepth > 0 {
		return tea.Batch(p.messages.ScrollToBottom(), sidebarCmd)
	}

	// Outermost stream stopped — fully clean up.
	// Only play the success sound when the stream completed normally.
	// Errors already trigger a failure sound via ErrorEvent, and
	// user-initiated cancels don't warrant a chime.
	if userconfig.Get().GetSound() && isSuccessfulStop(msg.Reason) {
		duration := time.Since(p.streamStartTime)
		threshold := time.Duration(userconfig.Get().GetSoundThreshold()) * time.Second
		if duration >= threshold {
			sound.Play(sound.Success)
		}
	}
	p.msgCancel = nil
	p.streamCancelled = false
	p.parentIdleDepth = 0
	spinnerCmd := p.setWorking(false)
	p.setPendingResponse(false)
	queueCmd := p.processNextQueuedMessage()

	var exitCmd tea.Cmd
	if p.app.ShouldExitAfterFirstResponse() && p.hasReceivedAssistantContent {
		slog.Debug("Exit after first response triggered, scheduling delayed exit")
		exitCmd = tea.Tick(50*time.Millisecond, func(time.Time) tea.Msg {
			return msgtypes.ExitAfterFirstResponseMsg{}
		})
	}

	return tea.Batch(p.messages.ScrollToBottom(), spinnerCmd, sidebarCmd, queueCmd, exitCmd)
}

// handlePartialToolCall processes partial tool call events by rendering each
// tool call as it streams in. The tool call appears with its name and a static
// "pending" indicator (not animated) to show it's receiving data.
func (p *chatPage) handlePartialToolCall(msg *runtime.PartialToolCallEvent) tea.Cmd {
	p.setPendingResponse(false)
	var toolDef tools.Tool
	if msg.ToolDefinition != nil {
		toolDef = *msg.ToolDefinition
	}
	toolCmd := p.messages.AddOrUpdateToolCall(msg.AgentName, msg.ToolCall, toolDef, types.ToolStatusPending)
	return tea.Batch(toolCmd, p.messages.ScrollToBottom())
}

func (p *chatPage) handleToolCallConfirmation(msg *runtime.ToolCallConfirmationEvent) tea.Cmd {
	spinnerCmd := p.setWorking(false)
	toolCmd := p.messages.AddOrUpdateToolCall(msg.AgentName, msg.ToolCall, msg.ToolDefinition, types.ToolStatusConfirmation)
	dialogCmd := core.CmdHandler(dialog.OpenDialogMsg{
		Model:            dialog.NewToolConfirmationDialog(msg, p.sessionState),
		OriginatingEvent: msg,
	})
	return tea.Batch(toolCmd, p.messages.ScrollToBottom(), spinnerCmd, dialogCmd)
}

func (p *chatPage) handleToolCall(msg *runtime.ToolCallEvent) tea.Cmd {
	p.setPendingResponse(false)
	spinnerCmd := p.setWorking(true)
	sidebarCmd := p.forwardToSidebar(msg)
	toolCmd := p.messages.AddOrUpdateToolCall(msg.AgentName, msg.ToolCall, msg.ToolDefinition, types.ToolStatusRunning)
	return tea.Batch(toolCmd, p.messages.ScrollToBottom(), spinnerCmd, sidebarCmd)
}

func (p *chatPage) handleToolCallResponse(msg *runtime.ToolCallResponseEvent) tea.Cmd {
	spinnerCmd := p.setWorking(true)
	sidebarCmd := p.forwardToSidebar(msg)

	status := types.ToolStatusCompleted
	if msg.Result.IsError {
		status = types.ToolStatusError
	}
	toolCmd := p.messages.AddToolResult(msg, status)

	// Update todo sidebar if this is a todo tool
	if msg.ToolDefinition.Category == "todo" && !msg.Result.IsError {
		_ = p.sidebar.SetTodos(msg.Result)
	}

	return tea.Batch(toolCmd, p.messages.ScrollToBottom(), spinnerCmd, sidebarCmd)
}

func (p *chatPage) handleMaxIterationsReached(msg *runtime.MaxIterationsReachedEvent) tea.Cmd {
	spinnerCmd := p.setWorking(false)
	dialogCmd := core.CmdHandler(dialog.OpenDialogMsg{
		Model:            dialog.NewMaxIterationsDialog(msg.MaxIterations, p.app),
		OriginatingEvent: msg,
	})
	return tea.Batch(spinnerCmd, dialogCmd)
}

func (p *chatPage) handleElicitationRequest(msg *runtime.ElicitationRequestEvent) tea.Cmd {
	spinnerCmd := p.setWorking(false)

	// Check if this is an OAuth flow by looking at the meta type
	// Guard against nil Meta map to prevent panic
	if msg.Meta != nil {
		if elicitationType, ok := msg.Meta["cagent/type"].(string); ok && elicitationType == "oauth_flow" {
			// OAuth flow - show the OAuth authorization dialog
			var serverURL string
			if url, ok := msg.Meta["cagent/server_url"].(string); ok {
				serverURL = url
			}
			dialogCmd := core.CmdHandler(dialog.OpenDialogMsg{
				Model:            dialog.NewOAuthAuthorizationDialog(serverURL, p.app),
				OriginatingEvent: msg,
			})
			return tea.Batch(spinnerCmd, dialogCmd)
		}
	}

	// Check elicitation mode
	switch msg.Mode {
	case "url":
		// URL-based elicitation - show URL dialog
		dialogCmd := core.CmdHandler(dialog.OpenDialogMsg{
			Model:            dialog.NewURLElicitationDialog(msg.Message, msg.URL),
			OriginatingEvent: msg,
		})
		return tea.Batch(spinnerCmd, dialogCmd)

	default:
		// Form-based elicitation (default) - show form dialog
		dialogCmd := core.CmdHandler(dialog.OpenDialogMsg{
			Model:            dialog.NewElicitationDialog(msg.Message, msg.Schema, msg.Meta),
			OriginatingEvent: msg,
		})
		return tea.Batch(spinnerCmd, dialogCmd)
	}
}

// isSuccessfulStop returns true when the stream reason indicates a
// normal completion that warrants the success sound. Empty reason
// (e.g. cache hits, early exits before a turn runs) is treated as
// success to preserve backward compatibility.
func isSuccessfulStop(reason string) bool {
	switch reason {
	case "", "normal", "continue", "steered":
		return true
	default:
		return false
	}
}
