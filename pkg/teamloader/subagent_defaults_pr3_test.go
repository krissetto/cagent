package teamloader

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
)

func TestLoadAppliesAgentLevelSubagentSpec(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "dummy")
	yaml := []byte(`
agents:
  root:
    model: openai/gpt-4o-mini
    instruction: root
    subagents:
      - name: scout
        agent: reviewer
        description: bounded discovery
  reviewer:
    model: openai/gpt-4o-mini
    instruction: reviewer
`)

	teams, err := Load(t.Context(), config.NewBytesSource("agent.yaml", yaml), &config.RuntimeConfig{})
	require.NoError(t, err)
	require.NotNil(t, teams)

	root, err := teams.Agent("root")
	require.NoError(t, err)
	require.Len(t, root.SubAgentSpecs(), 1)
	assert.Equal(t, "scout", root.SubAgentSpecs()[0].Name)
	assert.Equal(t, "reviewer", root.SubAgentSpecs()[0].Agent)
	assert.Equal(t, "bounded discovery", root.SubAgentSpecs()[0].Description)
	assert.NotZero(t, root.SubAgents())
}
