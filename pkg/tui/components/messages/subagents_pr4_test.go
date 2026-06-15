package messages

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

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
