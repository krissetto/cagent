package chat

import (
	"fmt"
	"log/slog"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/cagent/pkg/runtime"
	"github.com/docker/cagent/pkg/tui/components/notification"
	"github.com/docker/cagent/pkg/tui/components/sidebar"
	"github.com/docker/cagent/pkg/tui/core"
	"github.com/docker/cagent/pkg/tui/dialog"
	"github.com/docker/cagent/pkg/tui/types"
)

// handleRuntimeEvent processes runtime events and returns the appropriate command.
// Returns (handled, cmd) where handled indicates if the event was processed.
func (p *chatPage) handleRuntimeEvent(msg tea.Msg) (bool, tea.Cmd) {
	switch msg := msg.(type) {
	case *runtime.ErrorEvent:
		return true, p.messages.AddErrorMessage(msg.Error)

	case *runtime.ShellOutputEvent:
		return true, p.messages.AddShellOutputMessage(msg.Output)

	case *runtime.WarningEvent:
		return true, notification.WarningCmd(msg.Message)

	case *runtime.RAGIndexingStartedEvent,
		*runtime.RAGIndexingProgressEvent,
		*runtime.RAGIndexingCompletedEvent:
		return true, p.forwardToSidebar(msg)

	case *runtime.UserMessageEvent:
		return true, p.messages.ReplaceLoadingWithUser(msg.Message)

	case *runtime.StreamStartedEvent:
		return true, p.handleStreamStarted(msg)

	case *runtime.AgentChoiceEvent:
		return true, p.handleAgentChoice(msg)

	case *runtime.AgentChoiceReasoningEvent:
		return true, p.handleAgentChoiceReasoning(msg)

	case *runtime.TokenUsageEvent:
		p.sidebar.SetTokenUsage(msg)
		return true, nil

	case *runtime.SessionCompactionEvent:
		if msg.Status == "completed" {
			return true, notification.SuccessCmd("Session compacted successfully.")
		}
		return true, nil

	case *runtime.AgentInfoEvent:
		p.sidebar.SetAgentInfo(msg.AgentName, msg.Model, msg.Description)
		p.messages.AddWelcomeMessage(msg.WelcomeMessage)
		return true, nil

	case *runtime.TeamInfoEvent:
		p.sidebar.SetTeamInfo(msg.AvailableAgents)
		return true, nil

	case *runtime.AgentSwitchingEvent:
		p.sidebar.SetAgentSwitching(msg.Switching)
		return true, nil

	case *runtime.ToolsetInfoEvent:
		p.sidebar.SetToolsetInfo(msg.AvailableTools, msg.Loading)
		return true, nil

	case *runtime.StreamStoppedEvent:
		return true, p.handleStreamStopped(msg)

	case *runtime.SessionTitleEvent:
		return true, p.forwardToSidebar(msg)

	case *runtime.PartialToolCallEvent:
		return true, p.handlePartialToolCall(msg)

	case *runtime.ToolCallConfirmationEvent:
		return true, p.handleToolCallConfirmation(msg)

	case *runtime.ToolCallEvent:
		return true, p.handleToolCall(msg)

	case *runtime.ToolCallResponseEvent:
		return true, p.handleToolCallResponse(msg)

	case *runtime.MaxIterationsReachedEvent:
		return true, p.handleMaxIterationsReached(msg)

	case *runtime.ElicitationRequestEvent:
		return true, p.handleElicitationRequest(msg)

	// Task lifecycle events (kruntime mode) - forward to sidebar for display
	case *runtime.TaskStartedEvent,
		*runtime.TaskStateUpdatedEvent,
		*runtime.TaskWaitingEvent,
		*runtime.TaskCompletedEvent,
		*runtime.TaskUsageEvent:
		return true, p.forwardToSidebar(msg)
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

func (p *chatPage) handleStreamStarted(msg *runtime.StreamStartedEvent) tea.Cmd {
	p.streamCancelled = false
	spinnerCmd := p.setWorking(true)
	assistantCmd := p.messages.AddAssistantMessage()
	p.startProgressBar()
	sidebarCmd := p.forwardToSidebar(msg)
	return tea.Batch(assistantCmd, spinnerCmd, sidebarCmd)
}

func (p *chatPage) handleAgentChoice(msg *runtime.AgentChoiceEvent) tea.Cmd {
	if p.streamCancelled {
		return nil
	}
	return p.messages.AppendToLastMessage(msg.AgentName, types.MessageTypeAssistant, msg.Content)
}

func (p *chatPage) handleAgentChoiceReasoning(msg *runtime.AgentChoiceReasoningEvent) tea.Cmd {
	if p.streamCancelled {
		return nil
	}
	return p.messages.AppendToLastMessage(msg.AgentName, types.MessageTypeAssistantReasoning, msg.Content)
}

func (p *chatPage) handleStreamStopped(msg *runtime.StreamStoppedEvent) tea.Cmd {
	spinnerCmd := p.setWorking(false)
	if p.msgCancel != nil {
		p.msgCancel = nil
	}
	p.streamCancelled = false
	p.stopProgressBar()
	sidebarCmd := p.forwardToSidebar(msg)
	return tea.Batch(p.messages.ScrollToBottom(), spinnerCmd, sidebarCmd)
}

func (p *chatPage) handlePartialToolCall(msg *runtime.PartialToolCallEvent) tea.Cmd {
	spinnerCmd := p.setWorking(true)
	toolCmd := p.messages.AddOrUpdateToolCall(msg.AgentName, msg.ToolCall, msg.ToolDefinition, types.ToolStatusPending)
	return tea.Batch(toolCmd, p.messages.ScrollToBottom(), spinnerCmd)
}

func (p *chatPage) handleToolCallConfirmation(msg *runtime.ToolCallConfirmationEvent) tea.Cmd {
	spinnerCmd := p.setWorking(false)
	toolCmd := p.messages.AddOrUpdateToolCall(msg.AgentName, msg.ToolCall, msg.ToolDefinition, types.ToolStatusConfirmation)
	dialogCmd := core.CmdHandler(dialog.OpenDialogMsg{
		Model: dialog.NewToolConfirmationDialog(msg, p.sessionState),
	})
	return tea.Batch(toolCmd, p.messages.ScrollToBottom(), spinnerCmd, dialogCmd)
}

func (p *chatPage) handleToolCall(msg *runtime.ToolCallEvent) tea.Cmd {
	spinnerCmd := p.setWorking(true)
	toolCmd := p.messages.AddOrUpdateToolCall(msg.AgentName, msg.ToolCall, msg.ToolDefinition, types.ToolStatusRunning)
	return tea.Batch(toolCmd, p.messages.ScrollToBottom(), spinnerCmd)
}

func (p *chatPage) handleToolCallResponse(msg *runtime.ToolCallResponseEvent) tea.Cmd {
	spinnerCmd := p.setWorking(true)

	status := types.ToolStatusCompleted
	if msg.Result.IsError {
		status = types.ToolStatusError
	}
	toolCmd := p.messages.AddToolResult(msg, status)

	// Update todo sidebar if this is a todo tool
	if msg.ToolDefinition.Category == "todo" && !msg.Result.IsError {
		_ = p.sidebar.SetTodos(msg.Result)
	}

	return tea.Batch(toolCmd, p.messages.ScrollToBottom(), spinnerCmd)
}

func (p *chatPage) handleMaxIterationsReached(msg *runtime.MaxIterationsReachedEvent) tea.Cmd {
	spinnerCmd := p.setWorking(false)
	dialogCmd := core.CmdHandler(dialog.OpenDialogMsg{
		Model: dialog.NewMaxIterationsDialog(msg.MaxIterations, p.app),
	})
	return tea.Batch(spinnerCmd, dialogCmd)
}

func (p *chatPage) handleElicitationRequest(msg *runtime.ElicitationRequestEvent) tea.Cmd {
	// TODO: handle normal elicitation requests
	spinnerCmd := p.setWorking(false)

	serverURL := msg.Meta["cagent/server_url"].(string)
	dialogCmd := core.CmdHandler(dialog.OpenDialogMsg{
		Model: dialog.NewOAuthAuthorizationDialog(serverURL, p.app),
	})

	return tea.Batch(spinnerCmd, dialogCmd)
}
