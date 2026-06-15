package runtime

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestSubagentStartToolCreatesRuntimeManagedChildAndListEntry(t *testing.T) {
	root := agent.New("root", "root", agent.WithModel(&mockProvider{id: "test/root"}))
	child := agent.New("implementer", "child", agent.WithModel(&mockProvider{id: "test/implementer"}))
	agent.WithSubAgents(child)(root)
	agent.WithSubAgentSpecs(agent.SubAgentSpec{Name: "implementer", Agent: "implementer", Description: "Low-risk code edits"})(root)

	rt, err := NewLocalRuntime(team.New(team.WithAgents(root, child)), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	parent := session.New(session.WithID("root-session"), session.WithAgentName("root"))
	require.NoError(t, rt.sessionStore.UpdateSession(t.Context(), parent))
	result, err := rt.handleSubagentTool(t.Context(), parent, ToolNameSubagentStart, tools.ToolCall{
		Function: tools.FunctionCall{Name: ToolNameSubagentStart, Arguments: `{"agent":"implementer","task":"edit a file"}`},
	})
	require.NoError(t, err)
	require.False(t, result.IsError, result.Output)

	var started struct {
		ID    string `json:"id"`
		Agent string `json:"agent_name"`
		State string `json:"state"`
	}
	require.NoError(t, json.Unmarshal([]byte(result.Output), &started))
	assert.NotEmpty(t, started.ID)
	assert.Equal(t, "implementer", started.Agent)
	assert.Equal(t, "running", started.State)

	list, err := rt.handleSubagentTool(t.Context(), parent, ToolNameSubagentList, tools.ToolCall{
		Function: tools.FunctionCall{Name: ToolNameSubagentList, Arguments: `{}`},
	})
	require.NoError(t, err)
	require.False(t, list.IsError, list.Output)
	assert.Contains(t, list.Output, started.ID)
	assert.Contains(t, list.Output, "implementer")

	stored, err := rt.sessionStore.GetSession(t.Context(), started.ID)
	require.NoError(t, err)
	assert.Equal(t, "implementer", stored.AgentName)
	assert.Equal(t, parent.ID, stored.ParentID)
	assert.True(t, stored.RuntimeManaged)

	require.NoError(t, rt.subagents.Stop(parent, started.ID))
}
