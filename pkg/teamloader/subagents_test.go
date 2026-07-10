package teamloader

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/js"
	"github.com/docker/docker-agent/pkg/subagent"
)

func TestGetToolsForAgent_SubagentsInjectsAsyncTools(t *testing.T) {
	t.Parallel()

	a := &latest.AgentConfig{
		Name:      "root",
		Subagents: latest.SubagentRefs{{Agent: "worker", Description: "Does work"}},
	}
	runConfig := config.RuntimeConfig{EnvProviderForTests: &noEnvProvider{}}
	expander := js.NewJsExpander(runConfig.EnvProvider())

	toolSets, warnings := getToolsForAgent(t.Context(), a, ".", &runConfig, &toolsetRegistry{}, "test-config", expander)
	require.Empty(t, warnings)
	require.Len(t, toolSets, 1)

	tools, err := toolSets[0].Tools(t.Context())
	require.NoError(t, err)
	var names []string
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	assert.ElementsMatch(t, []string{subagent.ToolSpawnSubagent, subagent.ToolSendMessage, subagent.ToolReadSubagent, subagent.ToolStopSubagent}, names)
	assert.Contains(t, toolSets[0].(interface{ Instructions() string }).Instructions(), "worker: Does work")
}

func TestLoadWithConfig_SubagentsAreAvailableOnAgent(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "dummy")

	data := []byte(`agents:
  root:
    model: openai/gpt-4o
    instruction: coordinate
    subagents:
      - agent: worker
        name: work
        description: Does work
  worker:
    model: openai/gpt-4o
    instruction: do work
`)

	result, err := LoadWithConfig(t.Context(), config.NewBytesSource("async-subagents.yaml", data), &config.RuntimeConfig{}, withTestProviderRegistry()...)
	require.NoError(t, err)

	root, err := result.Team.Agent("root")
	require.NoError(t, err)
	require.Len(t, root.AsyncSubagents(), 1)
	assert.Equal(t, "worker", root.AsyncSubagents()[0].Agent)
	assert.Equal(t, "work", root.AsyncSubagents()[0].Name)
}
