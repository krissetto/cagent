package teamloader

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/tools/builtin/subagents"
	"github.com/docker/docker-agent/pkg/tools/builtin/transfertask"
)

func TestLoadSubagentsExposeRuntimeManagedToolsNotTransferTask(t *testing.T) {
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
	availableTools, err := root.Tools(t.Context())
	require.NoError(t, err)

	toolNames := make([]string, 0, len(availableTools))
	for _, tool := range availableTools {
		toolNames = append(toolNames, tool.Name)
	}

	assert.Contains(t, toolNames, subagents.ToolNameSubagentStart)
	assert.Contains(t, toolNames, subagents.ToolNameSubagentSend)
	assert.Contains(t, toolNames, subagents.ToolNameSubagentInspect)
	assert.Contains(t, toolNames, subagents.ToolNameSubagentList)
	assert.Contains(t, toolNames, subagents.ToolNameSubagentStop)
	assert.Contains(t, toolNames, subagents.ToolNameSubagentFinalize)
	assert.NotContains(t, toolNames, transfertask.ToolNameTransferTask)
}

func TestLoadRuntimeManagedSubagentToolsAreAbsentWithoutConfiguredSubagents(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "dummy")
	yaml := []byte(`
agents:
  root:
    model: openai/gpt-4o-mini
    instruction: root
  reviewer:
    model: openai/gpt-4o-mini
    instruction: reviewer
`)

	teams, err := Load(t.Context(), config.NewBytesSource("agent.yaml", yaml), &config.RuntimeConfig{})
	require.NoError(t, err)

	root, err := teams.Agent("root")
	require.NoError(t, err)
	availableTools, err := root.Tools(t.Context())
	require.NoError(t, err)

	toolNames := make([]string, 0, len(availableTools))
	for _, tool := range availableTools {
		toolNames = append(toolNames, tool.Name)
	}

	assert.NotContains(t, toolNames, subagents.ToolNameSubagentStart)
	assert.NotContains(t, toolNames, subagents.ToolNameSubagentSend)
	assert.NotContains(t, toolNames, subagents.ToolNameSubagentInspect)
	assert.NotContains(t, toolNames, subagents.ToolNameSubagentList)
	assert.NotContains(t, toolNames, subagents.ToolNameSubagentStop)
	assert.NotContains(t, toolNames, subagents.ToolNameSubagentFinalize)
	assert.NotContains(t, toolNames, transfertask.ToolNameTransferTask)
}

func TestLoadTransferAgentsExposeTransferTaskNotRuntimeManagedSubagentTools(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "dummy")
	yaml := []byte(`
agents:
  root:
    model: openai/gpt-4o-mini
    instruction: root
    transfer_agents: [reviewer]
  reviewer:
    model: openai/gpt-4o-mini
    instruction: reviewer
`)

	teams, err := Load(t.Context(), config.NewBytesSource("agent.yaml", yaml), &config.RuntimeConfig{})
	require.NoError(t, err)

	root, err := teams.Agent("root")
	require.NoError(t, err)
	availableTools, err := root.Tools(t.Context())
	require.NoError(t, err)

	toolNames := make([]string, 0, len(availableTools))
	for _, tool := range availableTools {
		toolNames = append(toolNames, tool.Name)
	}

	assert.Contains(t, toolNames, transfertask.ToolNameTransferTask)
	assert.NotContains(t, toolNames, subagents.ToolNameSubagentStart)
	assert.NotContains(t, toolNames, subagents.ToolNameSubagentInspect)
}

func TestLoadSubagentsAndTransferAgentsExposeSeparateToolContracts(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "dummy")
	yaml := []byte(`
agents:
  root:
    model: openai/gpt-4o-mini
    instruction: root
    subagents: [implementer]
    transfer_agents: [reviewer]
  implementer:
    model: openai/gpt-4o-mini
    instruction: implementer
  reviewer:
    model: openai/gpt-4o-mini
    instruction: reviewer
`)

	teams, err := Load(t.Context(), config.NewBytesSource("agent.yaml", yaml), &config.RuntimeConfig{})
	require.NoError(t, err)

	root, err := teams.Agent("root")
	require.NoError(t, err)
	availableTools, err := root.Tools(t.Context())
	require.NoError(t, err)

	toolNames := make([]string, 0, len(availableTools))
	for _, tool := range availableTools {
		toolNames = append(toolNames, tool.Name)
	}

	assert.Contains(t, toolNames, subagents.ToolNameSubagentStart)
	assert.Contains(t, toolNames, subagents.ToolNameSubagentInspect)
	assert.Contains(t, toolNames, transfertask.ToolNameTransferTask)
}
