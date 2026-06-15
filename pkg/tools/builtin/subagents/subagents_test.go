package subagents

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
)

func TestToolDescriptionsTeachWaitNotPoll(t *testing.T) {
	t.Parallel()

	child := agent.New("implementer", "", agent.WithDescription("edits code"))
	root := agent.New("root", "", agent.WithSubAgents(child))
	tools, err := New(root).Tools(t.Context())
	require.NoError(t, err)

	descriptions := map[string]string{}
	for _, tool := range tools {
		descriptions[tool.Name] = tool.Description
	}

	assert.Contains(t, descriptions[ToolNameSubagentStart], "end your turn and wait")
	assert.Contains(t, descriptions[ToolNameSubagentStart], "do not poll")
	assert.Contains(t, descriptions[ToolNameSubagentStart], "turn boundaries")
	assert.Contains(t, descriptions[ToolNameSubagentStart], "parks to wait on its own descendants")
	assert.Contains(t, descriptions[ToolNameSubagentStart], "short previews")
	assert.Contains(t, descriptions[ToolNameSubagentStart], "Available subagents")
	assert.Contains(t, descriptions[ToolNameSubagentStart], "implementer")
	assert.Contains(t, descriptions[ToolNameSubagentInspect], "short previews")
	assert.Contains(t, descriptions[ToolNameSubagentInspect], "when truncated")
	assert.Contains(t, descriptions[ToolNameSubagentInspect], "full latest message")
	assert.Contains(t, descriptions[ToolNameSubagentInspect], "Do not use this to poll")
	assert.Contains(t, descriptions[ToolNameSubagentList], "Do not use this to poll")
	assert.Contains(t, descriptions[ToolNameSubagentSend], "instead of sending polling messages")
}
