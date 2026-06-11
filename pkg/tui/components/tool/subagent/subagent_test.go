package subagent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestRenderStartCompletedShowsAgentAndShortRef(t *testing.T) {
	t.Parallel()

	msg := toolMessage(runtime.ToolNameSubagentStart, `{"agent":"reviewer","task":"check"}`, `{"subagent_id":"abcdef12345"}`, types.ToolStatusCompleted)
	m := New(msg, &service.SessionState{}).(*model)

	view := m.View()
	assert.Contains(t, view, "asking")
	assert.Contains(t, view, "reviewer")
	assert.Contains(t, view, "abcde")
	require.Equal(t, "abcde", m.SubAgentShortRef())
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
