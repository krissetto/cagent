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
