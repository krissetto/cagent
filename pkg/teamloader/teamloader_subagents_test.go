package teamloader

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin"
)

// flattenToolNames returns every tool name exposed by every toolset in the
// provided slice. Used by the migration tests below to make presence/absence
// assertions for specific tool names.
func flattenToolNames(t *testing.T, sets []tools.ToolSet) []string {
	t.Helper()
	var names []string
	for _, s := range sets {
		ts, err := s.Tools(t.Context())
		require.NoError(t, err)
		for _, tool := range ts {
			names = append(names, tool.Name)
		}
	}
	return names
}

// TestGetToolsForAgent_SubagentsFieldEnablesRuntimeManagedSurface verifies that
// the top-level `subagents:` field is the sole opt-in for the runtime-managed
// subagent subsystem: it must expose the six `subagent_*` tools and must not
// re-inject the legacy `transfer_task` / `handoff` surfaces.
func TestGetToolsForAgent_SubagentsFieldEnablesRuntimeManagedSurface(t *testing.T) {
	t.Parallel()

	a := &latest.AgentConfig{
		Instruction: "test",
		SubAgents:   []string{"helper"},
	}

	runConfig := config.RuntimeConfig{EnvProviderForTests: &noEnvProvider{}}

	got, warnings := getToolsForAgent(t.Context(), a, ".", &runConfig, NewDefaultToolsetRegistry(), "test-config")
	require.Empty(t, warnings)
	require.NotEmpty(t, got)

	names := flattenToolNames(t, got)
	assert.Contains(t, names, "subagent_start")
	assert.Contains(t, names, "subagent_send")
	assert.Contains(t, names, "subagent_list")
	assert.Contains(t, names, "subagent_inspect")
	assert.Contains(t, names, "subagent_finalize")
	assert.NotContains(t, names, "subagent_close")
	assert.Contains(t, names, "subagent_stop")
	assert.NotContains(t, names, builtin.ToolNameTransferTask,
		"subagents: must not also auto-inject the legacy transfer_task tool")
	assert.NotContains(t, names, builtin.ToolNameHandoff,
		"subagents: must not also auto-inject the legacy handoff tool")
}

// TestGetToolsForAgent_NoSubagentsFieldSkipsRuntimeSurface verifies that when
// an agent does not list subagents the runtime-managed subagent tools are not
// injected. This is the "plain agent" baseline.
func TestGetToolsForAgent_NoSubagentsFieldSkipsRuntimeSurface(t *testing.T) {
	t.Parallel()

	a := &latest.AgentConfig{
		Instruction: "test",
	}

	runConfig := config.RuntimeConfig{EnvProviderForTests: &noEnvProvider{}}

	got, _ := getToolsForAgent(t.Context(), a, ".", &runConfig, NewDefaultToolsetRegistry(), "test-config")

	names := flattenToolNames(t, got)
	assert.NotContains(t, names, "subagent_start")
	assert.NotContains(t, names, builtin.ToolNameTransferTask)
	assert.NotContains(t, names, builtin.ToolNameHandoff)
}

// TestGetToolsForAgent_HandoffsStillInjectsHandoffTool verifies that the
// legacy `handoffs:` auto-injection still works on its own for agents that
// are not using the runtime-managed subagent subsystem.
func TestGetToolsForAgent_HandoffsStillInjectsHandoffTool(t *testing.T) {
	t.Parallel()

	a := &latest.AgentConfig{
		Instruction: "test",
		Handoffs:    []string{"peer"},
	}

	runConfig := config.RuntimeConfig{EnvProviderForTests: &noEnvProvider{}}

	got, warnings := getToolsForAgent(t.Context(), a, ".", &runConfig, NewDefaultToolsetRegistry(), "test-config")
	require.Empty(t, warnings)
	require.NotEmpty(t, got)

	names := flattenToolNames(t, got)
	assert.Contains(t, names, builtin.ToolNameHandoff)
	assert.NotContains(t, names, "subagent_start")
}
