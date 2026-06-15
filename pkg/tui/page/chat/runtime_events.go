package chat

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	chatmsg "github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/sound"
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
// pattern of explicit event-to-update mappings. Events are organized by category:
//
// Stream Lifecycle:
//   - StreamStartedEvent  → Start spinners, set pending response
//   - StreamStoppedEvent  → Stop spinners, process queue, maybe exit
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
//   - ToolCallOutputEvent       → Append live tool output
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

	// ===== Content Events =====
	case *runtime.UserMessageEvent:
		if p.alreadyHydratedPosition(msg.SessionPosition) {
			return true, nil
		}
		if msg.Kind == session.MessageKindSubagentEnvelope || looksLikeRawSubagentEnvelope(msg.Message) {
			return true, p.messages.AddSubAgentMessage(subAgentInfoFromText(msg.Message))
		}
		return true, p.messages.ReplaceLoadingWithUser(msg.Message, msg.SessionPosition)

	case *runtime.SubAgentStartedEvent:
		msgCmd := p.messages.AddSubAgentMessage(types.SubAgentInfo{Kind: types.SubAgentEventStarted, AgentName: msg.SubAgent.AgentName, ShortID: msg.SubAgent.ShortID})
		return true, tea.Batch(msgCmd, p.forwardToSidebar(msg))

	case *runtime.SubAgentSentEvent:
		msgCmd := p.messages.AddSubAgentMessage(types.SubAgentInfo{Kind: types.SubAgentEventSent, ShortID: sidebarShortID(msg.SubAgentID)})
		return true, tea.Batch(msgCmd, p.forwardToSidebar(msg))

	case *runtime.SubAgentUpdateEvent:
		msgCmd := p.messages.AddSubAgentMessage(subAgentInfoFromEnvelope(msg.Envelope))
		return true, tea.Batch(msgCmd, p.forwardToSidebar(msg))

	case *runtime.LiveSessionTreeChangedEvent:
		p.sidebar.SetLiveSessionTree(msg.Tree)
		return true, nil

	case *runtime.AgentChoiceEvent:
		return true, p.handleAgentChoice(msg)

	case *runtime.MessageAddedEvent:
		return true, p.handleMessageAdded(msg)

	case *runtime.AgentChoiceReasoningEvent:
		return true, p.handleAgentChoiceReasoning(msg)

	case *runtime.ShellOutputEvent:
		return true, p.messages.AddShellOutputMessage(msg.Output)

	// ===== Tool Events =====
	case *runtime.PartialToolCallEvent:
		return true, p.handlePartialToolCall(msg)

	case *runtime.ToolCallEvent:
		return true, p.handleToolCall(msg)

	case *runtime.ToolCallConfirmationEvent:
		return true, p.handleToolCallConfirmation(msg)

	case *runtime.ToolCallOutputEvent:
		return true, p.handleToolCallOutput(msg)

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

	case *runtime.ParentIdleEvent:
		p.refreshLiveSessionTree()
		cmds := []tea.Cmd{p.setWorking(false), p.setPendingResponse(false), p.flushQueuedFollowUps()}
		cmds = append(cmds, p.forwardToSidebar(msg))
		return true, tea.Batch(cmds...)

	case *runtime.ParentResumeEvent:
		p.refreshLiveSessionTree()
		return true, p.forwardToSidebar(msg)

	case *runtime.SessionQueueEvent:
		p.refreshLiveSessionTree()
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
	// Working state is owned by this page's own session. A start for another
	// session (e.g. a child whose events reached this page) must not drive the
	// parent's working spinner, otherwise an unmatched child start leaves the
	// parent spinning forever between messages.
	if !p.ownsSessionStream(msg.SessionID) {
		return p.forwardToSidebar(msg)
	}
	p.streamCancelled = false
	p.streamDepth++
	p.streamStartTime = time.Now()
	spinnerCmd := p.setWorking(true)
	pendingCmd := p.setPendingResponse(true)
	sidebarCmd := p.forwardToSidebar(msg)
	return tea.Batch(pendingCmd, spinnerCmd, sidebarCmd)
}

// ownsSessionStream reports whether a stream lifecycle event belongs to this
// page's own session and should drive its working/spinner state. Events with
// an empty session id come from the main loop and are treated as owned;
// events scoped to a different session (a descendant) are not.
func (p *chatPage) ownsSessionStream(sessionID string) bool {
	if sessionID == "" {
		return true
	}
	if p.app == nil || p.app.Session() == nil {
		return true
	}
	return sessionID == p.app.Session().ID
}

func (p *chatPage) alreadyHydratedPosition(sessionPos int) bool {
	if !p.app.IsAttachedLive() {
		return false
	}
	// Store history is the single transcript source for an attached session
	// (loaded once in chat.Init -> LoadFromSession). Any positioned event at or
	// below the hydration watermark is already on screen. Higher positions and
	// position-less in-flight streaming deltas (sessionPos < 0) are genuinely
	// new and render normally — nothing is lost.
	return sessionPos >= 0 && sessionPos <= p.messages.HydratedThroughPosition()
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

func (p *chatPage) handleMessageAdded(msg *runtime.MessageAddedEvent) tea.Cmd {
	if msg == nil || msg.Message == nil {
		return nil
	}
	m := msg.Message.Message
	agentName := msg.AgentName
	if agentName == "" {
		agentName = msg.Message.AgentName
	}
	switch m.Role {
	case chatmsg.MessageRoleUser:
		// A user message already loaded from store history (positioned at or
		// below the hydration watermark on a live attach) is already on screen.
		if p.alreadyHydratedPosition(msg.SessionPosition) {
			return nil
		}
		return p.messages.ReplaceLoadingWithUser(m.Content, msg.SessionPosition)
	case chatmsg.MessageRoleAssistant:
		// Assistant turns stream in via AgentChoiceEvent deltas; the
		// MessageAddedEvent is the persisted finalization of that same turn.
		// Merge it into the matching streamed message by position. This is
		// idempotent: identical content already on screen is a no-op, while a
		// fuller final replaces a partial. We do not gate this on the
		// hydration watermark so a finalized version can still correct a
		// partial that was loaded from store history. Only when there is no
		// matching message to finalize do we append.
		if p.messages.MergeAssistantFinal(agentName, m.Content, msg.SessionPosition) {
			return nil
		}
		if p.alreadyHydratedPosition(msg.SessionPosition) {
			return nil
		}
		return p.messages.AppendToLastMessage(agentName, m.Content)
	default:
		return nil
	}
}

func (p *chatPage) handleAgentChoiceReasoning(msg *runtime.AgentChoiceReasoningEvent) tea.Cmd {
	if p.streamCancelled {
		return nil
	}
	p.setPendingResponse(false)
	return p.messages.AppendReasoning(msg.AgentName, msg.Content)
}

func (p *chatPage) handleStreamStopped(msg *runtime.StreamStoppedEvent) tea.Cmd {
	slog.Debug("handleStreamStopped called",
		"agent", msg.AgentName,
		"session_id", msg.SessionID,
		"reason", msg.Reason,
		"should_exit", p.app.ShouldExitAfterFirstResponse(),
		"has_content", p.hasReceivedAssistantContent,
		"stream_depth", p.streamDepth)

	// Stops for descendant sessions don't affect this page's own working
	// state; just refresh the sidebar. This prevents cross-session stop
	// events from driving the parent counter negative or clearing the
	// parent spinner mid-turn.
	if !p.ownsSessionStream(msg.SessionID) {
		return p.forwardToSidebar(msg)
	}

	if p.streamDepth > 0 {
		p.streamDepth--
	}

	sidebarCmd := p.forwardToSidebar(msg)

	// Nested same-session stream stopped while an outer one is still active —
	// keep the working/cancel state intact and just refresh the sidebar.
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

func (p *chatPage) handleToolCallOutput(msg *runtime.ToolCallOutputEvent) tea.Cmd {
	return tea.Batch(p.messages.AppendToolOutput(msg), p.messages.ScrollToBottom())
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
		if elicitationType, ok := msg.Meta["docker-agent/type"].(string); ok && elicitationType == "oauth_flow" {
			// OAuth flow - show the OAuth authorization dialog
			var serverURL string
			if url, ok := msg.Meta["docker-agent/server_url"].(string); ok {
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

func looksLikeRawSubagentEnvelope(content string) bool {
	content = strings.TrimSpace(content)
	return strings.HasPrefix(content, "<subagent_update") || strings.HasPrefix(content, "<subagent_result") || strings.HasPrefix(content, "<subagent_envelope")
}

func subAgentInfoFromEnvelope(env runtime.SubagentEnvelope) types.SubAgentInfo {
	kind := subAgentEventKindFromString(env.Kind)
	detail := env.Preview
	if env.Error != "" {
		if detail != "" {
			detail += " "
		}
		detail += env.Error
	}
	return types.SubAgentInfo{Kind: kind, AgentName: env.AgentName, ShortID: sidebarShortID(env.SubAgentID), Detail: detail, Truncated: env.Truncated}
}

func subAgentInfoFromText(text string) types.SubAgentInfo {
	info := types.SubAgentInfo{Kind: types.SubAgentEventTurnCompleted, Detail: strings.TrimSpace(text)}
	content := info.Detail
	if content == "" {
		return info
	}

	if strings.HasPrefix(content, "<") {
		if attr := parseSubagentEnvelopeAttrs(content); attr != nil {
			info.AgentName = attr["agent_name"]
			info.ShortID = sidebarShortID(firstNonEmpty(attr["subagent_id"], attr["id"]))
			if kind := firstNonEmpty(attr["kind"], attr["status"]); kind != "" {
				info.Kind = subAgentEventKindFromString(kind)
			}
			info.Detail = strings.TrimSpace(firstNonEmpty(attr["preview"], attr["message"], attr["error"], content))
			return info
		}
	}

	switch {
	case strings.Contains(content, " finalized"):
		info.Kind = types.SubAgentEventClosed
	case strings.Contains(content, " stopped"):
		info.Kind = types.SubAgentEventStopped
	case strings.Contains(content, " failed"):
		info.Kind = types.SubAgentEventFailed
	}

	if end := strings.Index(content, "]"); strings.HasPrefix(content, "[") && end > 1 {
		info.AgentName = strings.TrimSpace(content[1:end])
		if rest := strings.TrimSpace(content[end+1:]); strings.HasPrefix(rest, "(") {
			if closeIdx := strings.Index(rest, ")"); closeIdx > 1 {
				info.ShortID = strings.TrimSpace(rest[1:closeIdx])
			}
		}
	}
	return info
}

func subAgentEventKindFromString(kind string) types.SubAgentEventKind {
	switch strings.TrimSpace(kind) {
	case "closed":
		return types.SubAgentEventClosed
	case "stopped":
		return types.SubAgentEventStopped
	case "failed":
		return types.SubAgentEventFailed
	default:
		return types.SubAgentEventTurnCompleted
	}
}

func parseSubagentEnvelopeAttrs(content string) map[string]string {
	end := strings.Index(content, ">")
	if end == -1 {
		return nil
	}
	fields := strings.Fields(strings.Trim(content[1:end], "/ "))
	if len(fields) == 0 {
		return nil
	}
	attrs := make(map[string]string)
	for _, field := range fields[1:] {
		key, val, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		attrs[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(val), "\"'")
	}
	return attrs
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func sidebarShortID(id string) string {
	id = strings.TrimSpace(id)
	if len(id) <= 5 {
		return id
	}
	return id[:5]
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
