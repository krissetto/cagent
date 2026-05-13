package subagent

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToolSetDoesNotAdvertiseDeprecatedCloseAlias(t *testing.T) {
	t.Parallel()

	ts := NewToolSet([]ToolSetAgentSpec{{Name: "planner"}, {Name: "coder"}})
	tools, err := ts.Tools(t.Context())
	require.NoError(t, err)

	var names []string
	for _, tool := range tools {
		names = append(names, tool.Name)
	}

	assert.Contains(t, names, ToolNameFinalize)
	assert.NotContains(t, names, ToolNameClose,
		"deprecated close alias should remain dispatchable but not advertised to the model")
}

func TestToolSetConstrainsStartAgentToConfiguredSubagents(t *testing.T) {
	t.Parallel()

	ts := NewToolSet([]ToolSetAgentSpec{
		{Name: "planner", Description: "Plan implementation"},
		{Name: "coder", Description: "Write code"},
	})
	tools, err := ts.Tools(t.Context())
	require.NoError(t, err)

	var startTool any
	for _, tool := range tools {
		if tool.Name == ToolNameStart {
			startTool = tool.Parameters
			assert.Contains(t, tool.Description, "Available sub-agents:")
			assert.Contains(t, tool.Description, "planner: Plan implementation")
			break
		}
	}
	require.NotNil(t, startTool)

	params, ok := startTool.(map[string]any)
	require.True(t, ok)
	props, ok := params["properties"].(map[string]any)
	require.True(t, ok)
	agentProp, ok := props["agent"].(map[string]any)
	require.True(t, ok)

	assert.Equal(t, []any{"planner", "coder"}, agentProp["enum"])

	// Verify Instructions also lists the constrained names.
	type instructioner interface{ Instructions() string }
	ins, ok := ts.(instructioner)
	require.True(t, ok)
	assert.Contains(t, ins.Instructions(), "Only these agent names may be passed to subagent_start.")
	assert.Contains(t, ins.Instructions(), "Runtime updates are push-based")
	assert.Contains(t, ins.Instructions(), "Keep delegated ownership clear")
	assert.Contains(t, ins.Instructions(), "Treat inspect as transcript retrieval")
	assert.NotContains(t, ins.Instructions(), "models reproduce")
}

func TestSubagentIDSchemaDescriptionsStayInSync(t *testing.T) {
	t.Parallel()

	types := []reflect.Type{
		reflect.TypeFor[SendArgs](),
		reflect.TypeFor[InspectArgs](),
		reflect.TypeFor[FinalizeArgs](),
		reflect.TypeFor[StopArgs](),
	}
	for _, typ := range types {
		field, ok := typ.FieldByName("SubAgentID")
		require.True(t, ok)
		assert.Equal(t, subagentIDDescription, field.Tag.Get("jsonschema"), typ.Name())
	}
}
