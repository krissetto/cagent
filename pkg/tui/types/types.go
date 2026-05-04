package types

import (
	"strings"
	"time"

	"github.com/docker/docker-agent/pkg/tools"
)

// MessageType represents different types of messages
type MessageType int

const (
	MessageTypeUser MessageType = iota
	MessageTypeAssistant
	MessageTypeAssistantReasoningBlock // Collapsed reasoning + tool calls block
	MessageTypeSpinner
	MessageTypeError
	MessageTypeShellOutput
	MessageTypeCancelled
	MessageTypeToolCall
	MessageTypeToolResult
	MessageTypeWelcome
	MessageTypeLoading
	// MessageTypeSubAgent renders a compact, model-agnostic card for a
	// runtime-managed subagent lifecycle event (started / sent / update /
	// closed / stopped / failed). Emitted by the chat page in response to
	// SubAgent* runtime events.
	MessageTypeSubAgent
)

const (
	UserMessageEditLabel      = "✎"
	AssistantMessageCopyLabel = "⎘"
)

// ToolStatus represents the status of a tool call
type ToolStatus int

const (
	ToolStatusPending ToolStatus = iota
	ToolStatusConfirmation
	ToolStatusRunning
	ToolStatusCompleted
	ToolStatusError
)

// Message represents a single message in the chat
type Message struct {
	Type           MessageType
	Content        string
	Sender         string                // Agent name for assistant messages
	ToolCall       tools.ToolCall        // Associated tool call for tool messages
	ToolDefinition tools.Tool            // Definition of the tool being called
	ToolStatus     ToolStatus            // Status for tool calls
	ToolResult     *tools.ToolCallResult // Result of tool call (when completed)
	// StartedAt records when a tool call entered ToolStatusRunning.
	// Used to display elapsed time for long-running tool calls.
	StartedAt *time.Time
	// SessionPosition is the index of this message in session.Messages (when known).
	// Used for operations like branching on edits.
	SessionPosition *int
	// SubAgent carries the data for a MessageTypeSubAgent card. Nil for all
	// other message types.
	SubAgent *SubAgentInfo
}

func Agent(typ MessageType, agentName, content string) *Message {
	return &Message{
		Type:    typ,
		Sender:  agentName,
		Content: strings.ReplaceAll(content, "\t", "    "),
	}
}

func ShellOutput(content string) *Message {
	return &Message{
		Type:    MessageTypeShellOutput,
		Content: strings.ReplaceAll(content, "\t", "    "),
	}
}

func Spinner() *Message {
	return &Message{
		Type: MessageTypeSpinner,
	}
}

func Error(content string) *Message {
	return &Message{
		Type:    MessageTypeError,
		Content: strings.ReplaceAll(content, "\t", "    "),
	}
}

func User(content string) *Message {
	return &Message{
		Type:    MessageTypeUser,
		Content: strings.ReplaceAll(content, "\t", "    "),
	}
}

func Cancelled() *Message {
	return &Message{
		Type: MessageTypeCancelled,
	}
}

func Welcome(content string) *Message {
	return &Message{
		Type:    MessageTypeWelcome,
		Content: strings.ReplaceAll(content, "\t", "    "),
	}
}

func ToolCallMessage(agentName string, toolCall tools.ToolCall, toolDef tools.Tool, status ToolStatus) *Message {
	msg := &Message{
		Type:           MessageTypeToolCall,
		Sender:         agentName,
		ToolCall:       toolCall,
		ToolDefinition: toolDef,
		ToolStatus:     status,
	}
	if status == ToolStatusRunning {
		now := time.Now()
		msg.StartedAt = &now
	}
	return msg
}

func Loading(description string) *Message {
	return &Message{
		Type:    MessageTypeLoading,
		Content: strings.ReplaceAll(description, "\t", "    "),
	}
}

// SubAgentEventKind classifies a subagent transcript card so the renderer can
// pick the right glyph and tone. Values intentionally mirror the subagent
// envelope kinds (plus a "started"/"sent" prefix for parent-side actions).
type SubAgentEventKind string

const (
	SubAgentEventStarted       SubAgentEventKind = "started"
	SubAgentEventSent          SubAgentEventKind = "sent"
	SubAgentEventTurnCompleted SubAgentEventKind = "turn_completed"
	SubAgentEventClosed        SubAgentEventKind = "closed"
	SubAgentEventStopped       SubAgentEventKind = "stopped"
	SubAgentEventFailed        SubAgentEventKind = "failed"
)

// SubAgentInfo carries the data needed to render a subagent transcript card.
type SubAgentInfo struct {
	Kind      SubAgentEventKind
	AgentName string // subagent name (e.g. "researcher")
	ShortID   string // short id exposed to the model (first 5 chars)
	Detail    string // one-line detail: task for started, preview for updates, message for sent, error for failed
	Truncated bool   // true if Detail was truncated by the runtime
}

// SubAgent constructs a subagent lifecycle message for the transcript.
func SubAgent(info SubAgentInfo) *Message {
	return &Message{
		Type:     MessageTypeSubAgent,
		SubAgent: &info,
	}
}
