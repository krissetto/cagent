package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

func TestSubagentStartUsesConfiguredSpecAlias(t *testing.T) {
	root := agent.New("root", "root", agent.WithModel(&mockProvider{id: "test/root"}))
	coder := agent.New("coder", "coder", agent.WithModel(&mockProvider{id: "test/coder"}))
	agent.WithSubAgents(coder)(root)
	agent.WithSubAgentSpecs(agent.SubAgentSpec{
		Name:        "implementer",
		Agent:       "coder",
		Description: "Low-risk code edits",
	})(root)

	rt, err := NewLocalRuntime(team.New(team.WithAgents(root, coder)), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	parent := session.New(session.WithID("root-session"), session.WithAgentName("root"))
	require.NoError(t, rt.sessionStore.UpdateSession(t.Context(), parent))
	h, err := rt.subagents.Start(t.Context(), parent, "implementer", "edit a file")
	require.NoError(t, err)

	assert.Equal(t, "implementer", h.agentName)
	assert.Equal(t, "coder", h.sess.AgentName)
}
