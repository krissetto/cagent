package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

func TestSubagentStartUsesParentSessionAgentForNestedDelegation(t *testing.T) {
	root := agent.New("root", "root", agent.WithModel(&mockProvider{id: "test/root"}))
	director := agent.New("director", "director", agent.WithModel(&mockProvider{id: "test/director"}))
	implementer := agent.New("implementer", "implementer", agent.WithModel(&mockProvider{id: "test/implementer"}))
	agent.WithSubAgents(director)(root)
	agent.WithSubAgentSpecs(agent.SubAgentSpec{Name: "director", Agent: "director"})(root)
	agent.WithSubAgents(implementer)(director)
	agent.WithSubAgentSpecs(agent.SubAgentSpec{Name: "implementer", Agent: "implementer"})(director)

	rt, err := NewLocalRuntime(team.New(team.WithAgents(root, director, implementer)), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	rootSession := session.New(session.WithID("root-session"), session.WithAgentName("root"))
	directorSession := session.NewRuntimeManagedSubSession(rootSession,
		session.WithID("director-session"),
		session.WithAgentName("director"),
	)
	require.NoError(t, rt.sessionStore.AddSession(t.Context(), rootSession))
	require.NoError(t, rt.sessionStore.AddSession(t.Context(), directorSession))

	h, err := rt.subagents.Start(t.Context(), directorSession, "implementer", "nested edit")
	require.NoError(t, err)
	defer func() { _ = rt.subagents.Stop(directorSession, h.id) }()

	assert.Equal(t, "implementer", h.agentName)
	assert.Equal(t, "implementer", h.sess.AgentName)
	assert.Equal(t, directorSession.ID, h.parent.ID)
	assert.Equal(t, rootSession.ID, h.sess.EffectiveRootID())
}

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
