package teamloader

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config"
)

func TestLoadWiresAgentLevelSubagentsSpecs(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "dummy")
	yaml := []byte(`
agents:
  root:
    model: openai/gpt-4o-mini
    instruction: root
    subagents:
      - reviewer
      - name: implementer
        agent: coder
        description: Low-risk code edits
        ttl: 15m
  reviewer:
    model: openai/gpt-4o-mini
    instruction: reviewer
  coder:
    model: openai/gpt-4o-mini
    instruction: coder
`)

	teams, err := Load(t.Context(), config.NewBytesSource("agent.yaml", yaml), &config.RuntimeConfig{})
	require.NoError(t, err)

	root, err := teams.Agent("root")
	require.NoError(t, err)
	assert.Len(t, root.SubAgents(), 2)
	assert.Equal(t, []string{"reviewer", "coder"}, agentNamesForTest(root.SubAgents()))

	specs := root.SubAgentSpecs()
	require.Len(t, specs, 2)
	assert.Equal(t, "reviewer", specs[0].Name)
	assert.Equal(t, "reviewer", specs[0].Agent)
	assert.Equal(t, "implementer", specs[1].Name)
	assert.Equal(t, "coder", specs[1].Agent)
	assert.Equal(t, "Low-risk code edits", specs[1].Description)
	assert.Equal(t, 15*time.Minute, specs[1].TTL)
}

func TestLoadRejectsDuplicateRuntimeSubagentNames(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "dummy")
	yaml := []byte(`
agents:
  root:
    model: openai/gpt-4o-mini
    instruction: root
    subagents:
      - reviewer
      - name: reviewer
        agent: coder
  reviewer:
    model: openai/gpt-4o-mini
    instruction: reviewer
  coder:
    model: openai/gpt-4o-mini
    instruction: coder
`)

	_, err := Load(t.Context(), config.NewBytesSource("agent.yaml", yaml), &config.RuntimeConfig{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `duplicate subagent name "reviewer"`)
}

func agentNamesForTest(agents []*agent.Agent) []string {
	names := make([]string, 0, len(agents))
	for _, a := range agents {
		names = append(names, a.Name())
	}
	return names
}
