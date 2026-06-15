package subagent

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestRenderStartCompletedShowsAgentAndShortRef(t *testing.T) {
	t.Parallel()

	msg := toolMessage(runtime.ToolNameSubagentStart, `{"agent":"reviewer","task":"check"}`, `{"subagent_id":"abcdef12345"}`, types.ToolStatusCompleted)
	m := New(msg, &service.SessionState{}).(*model)

	view := m.View()
	plain := ansi.Strip(view)
	assert.Contains(t, plain, "asking")
	assert.Contains(t, plain, "reviewer")
	assert.Contains(t, plain, "abcde")
	assert.NotContains(t, plain, "asking  reviewer", "agent name should render as plain colored text, not a padded badge")
	assert.NotContains(t, plain, "reviewer  (", "agent name should not carry badge padding before the id")
	require.Equal(t, "abcde", m.SubAgentShortRef())
}

func TestRenderStartUsesSamePlainAgentNameStyleAsSubagentEnvelope(t *testing.T) {
	t.Parallel()

	toolView := renderSubagentAction("asking", "director", "338dd")
	envelopeView := renderSubagentAction("", "director", "338dd")
	agentStyle := styles.AgentAccentStyleFor("director").Render("director")

	assert.Contains(t, toolView, agentStyle)
	assert.Contains(t, envelopeView, agentStyle)
	assert.NotContains(t, ansi.Strip(toolView), "  director  ")
	assert.NotContains(t, ansi.Strip(envelopeView), "  director  ")
}

func TestRenderInspectExpandsCompletedTranscript(t *testing.T) {
	t.Parallel()

	msg := toolMessage(runtime.ToolNameSubagentInspect, `{"subagent_id":"child123","mode":"last"}`, `{"agent":"greppy","last":"important finding"}`, types.ToolStatusCompleted)
	m := New(msg, &service.SessionState{}).(*model)

	collapsed := m.View()
	assert.NotContains(t, collapsed, "important finding")
	require.True(t, m.ToggleExpanded())
	expanded := m.View()
	assert.Contains(t, expanded, "important finding")
}

func toolMessage(name, args, content string, status types.ToolStatus) *types.Message {
	return &types.Message{
		Type:    types.MessageTypeToolCall,
		Content: content,
		ToolCall: tools.ToolCall{Function: tools.FunctionCall{
			Name:      name,
			Arguments: args,
		}},
		ToolStatus: status,
	}
}
