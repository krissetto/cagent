package teamloader

import (
	"testing"

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
      - director
      - agent: implementer
        name: implementer
        description: The bench maker handles clear, low-risk code edits directly and proves the work with targeted checks.
  director:
    model: openai/gpt-4o-mini
    instruction: director
  implementer:
    model: openai/gpt-4o-mini
    instruction: implementer
`)

	teams, err := Load(t.Context(), config.NewBytesSource("agent.yaml", yaml), &config.RuntimeConfig{})
	require.NoError(t, err)

	root, err := teams.Agent("root")
	require.NoError(t, err)
	assert.Len(t, root.SubAgents(), 2)
	assert.Equal(t, []string{"director", "implementer"}, agentNamesForTest(root.SubAgents()))

	specs := root.SubAgentSpecs()
	require.Len(t, specs, 2)
	assert.Equal(t, "director", specs[0].Name)
	assert.Equal(t, "director", specs[0].Agent)
	assert.Equal(t, "implementer", specs[1].Name)
	assert.Equal(t, "implementer", specs[1].Agent)
	assert.Equal(t, "The bench maker handles clear, low-risk code edits directly and proves the work with targeted checks.", specs[1].Description)
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
