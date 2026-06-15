package messages

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestSubagentLifecycleAndAssistantSeparatedBySingleBlankLine(t *testing.T) {
	t.Parallel()

	m := NewScrollableView(80, 24, &service.SessionState{}).(*model)
	m.SetSize(80, 24)

	msgs := []*types.Message{
		types.SubAgent(types.SubAgentInfo{Kind: types.SubAgentEventTurnCompleted, AgentName: "greppy", ShortID: "abcde"}),
		types.Agent(types.MessageTypeAssistant, "root", "assistant text"),
	}
	for _, msg := range msgs {
		m.messages = append(m.messages, msg)
		m.views = append(m.views, m.createMessageView(msg))
	}
	m.ensureAllItemsRendered()

	plainLines := make([]string, len(m.renderedLines))
	for i, line := range m.renderedLines {
		plainLines[i] = ansi.Strip(line)
	}
	plain := strings.Join(plainLines, "\n")
	require.Contains(t, plain, "greppy")
	require.Contains(t, plain, "assistant text")
	require.GreaterOrEqual(t, len(plainLines), 4)
	require.Contains(t, plainLines[0], "greppy")
	require.Empty(t, strings.TrimSpace(plainLines[1]), "subagent and assistant blocks should have exactly one blank separator line")
	require.Contains(t, plain, "assistant text")
	require.NotContains(t, plain, "turn finishedassistant text")
}

func TestToolRowsAndSubagentLifecycleRowsDoNotInsertExtraSeparatorsBetweenEachOther(t *testing.T) {
	t.Parallel()

	m := NewScrollableView(80, 24, &service.SessionState{}).(*model)
	m.SetSize(80, 24)

	msgs := []*types.Message{
		types.ToolCallMessage("root", tools.ToolCall{ID: "tool-1", Function: tools.FunctionCall{Name: "read_file"}}, tools.Tool{Name: "read_file"}, types.ToolStatusCompleted),
		types.SubAgent(types.SubAgentInfo{Kind: types.SubAgentEventStarted, AgentName: "greppy", ShortID: "abcde"}),
		types.ToolCallMessage("root", tools.ToolCall{ID: "tool-2", Function: tools.FunctionCall{Name: "write_file"}}, tools.Tool{Name: "write_file"}, types.ToolStatusCompleted),
	}
	for _, msg := range msgs {
		m.messages = append(m.messages, msg)
		m.views = append(m.views, m.createMessageView(msg))
	}
	m.ensureAllItemsRendered()

	require.False(t, m.needsSeparator(0), "tool-like tool/subagent rows should stay compact")
	require.False(t, m.needsSeparator(1), "tool-like subagent/tool rows should stay compact")

	plainLines := make([]string, len(m.renderedLines))
	for i, line := range m.renderedLines {
		plainLines[i] = ansi.Strip(line)
	}
	plain := strings.Join(plainLines, "\n")
	require.Contains(t, plain, "greppy")
	require.NotContains(t, plain, "\n\n")
}

func TestLoadFromSessionRendersSubagentEnvelopeBetweenAssistantMessages(t *testing.T) {
	t.Parallel()

	sess := session.New(session.WithID("root"))
	sess.AddMessage(&session.Message{AgentName: "root", Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "first assistant"}})
	sess.AddMessage(session.SubagentEnvelopeMessage("[director] (abc12) turn finished. Preview: done"))
	sess.AddMessage(&session.Message{AgentName: "root", Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "second assistant"}})

	m := NewScrollableView(100, 24, &service.SessionState{}).(*model)
	m.SetSize(100, 24)
	m.LoadFromSession(sess)
	m.ensureAllItemsRendered()

	plainLines := make([]string, len(m.renderedLines))
	for i, line := range m.renderedLines {
		plainLines[i] = ansi.Strip(line)
	}
	plain := strings.Join(plainLines, "\n")

	require.Contains(t, plain, "first assistant")
	require.Contains(t, plain, "director")
	require.Contains(t, plain, "abc12")
	require.Contains(t, plain, "turn finished")
	require.Contains(t, plain, "second assistant")
	require.Less(t, strings.Index(plain, "first assistant"), strings.Index(plain, "director"))
	require.Less(t, strings.Index(plain, "director"), strings.Index(plain, "second assistant"))
	require.NotContains(t, plain, "first assistantsecond assistant")
	require.NotContains(t, plain, "turn finishedsecond assistant")
}

// TestSubAgentMessageRemovesPendingSpinner is the regression for a static,
// non-spinning pending row left between messages after a subagent delegation.
// A pending spinner (added on StreamStarted) followed by a subagent envelope
// must not leave the spinner stranded: removeSpinner only removes a trailing
// spinner, so AddSubAgentMessage must clear it before appending.
func TestSubAgentMessageRemovesPendingSpinner(t *testing.T) {
	t.Parallel()

	m := NewScrollableView(80, 24, &service.SessionState{}).(*model)
	m.SetSize(80, 24)

	// Pending-response spinner is showing (as after StreamStarted).
	_ = m.AddAssistantMessage()
	require.Equal(t, types.MessageTypeSpinner, m.messages[len(m.messages)-1].Type)

	// A subagent envelope arrives.
	_ = m.AddSubAgentMessage(types.SubAgentInfo{Kind: types.SubAgentEventStarted, AgentName: "director", ShortID: "e546b"})

	// No spinner message must remain anywhere in the list.
	for _, msg := range m.messages {
		require.NotEqual(t, types.MessageTypeSpinner, msg.Type, "pending spinner must be removed when a subagent message is added")
	}
	require.Equal(t, types.MessageTypeSubAgent, m.messages[len(m.messages)-1].Type)
}
