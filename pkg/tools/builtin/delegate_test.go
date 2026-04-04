package builtin

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDelegateTool(t *testing.T) {
	toolset := NewDelegateTool()
	tools, err := toolset.Tools(context.Background())
	require.NoError(t, err)
	require.Len(t, tools, 3)

	assert.Equal(t, ToolNameDelegate, tools[0].Name)
	assert.Equal(t, ToolNameContinueDelegation, tools[1].Name)
	assert.Equal(t, ToolNameStopDelegation, tools[2].Name)
}

func TestDelegateToolSchemas(t *testing.T) {
	toolset := NewDelegateTool()
	toolList, err := toolset.Tools(context.Background())
	require.NoError(t, err)
	require.Len(t, toolList, 3)

	toMap := func(t *testing.T, params any) map[string]any {
		t.Helper()
		m, err := tools.SchemaToMap(params)
		require.NoError(t, err)
		return m
	}

	props := func(m map[string]any) map[string]any {
		return m["properties"].(map[string]any)
	}

	requiredKeys := func(m map[string]any) []any {
		r, _ := m["required"].([]any)
		return r
	}

	// Verify delegate tool has correct schema (agent, task)
	delegateSchema := toMap(t, toolList[0].Parameters)
	assert.Contains(t, props(delegateSchema), "agent")
	assert.Contains(t, props(delegateSchema), "task")
	assert.ElementsMatch(t, requiredKeys(delegateSchema), []any{"agent", "task"})

	// Verify continue_delegation tool has correct schema (delegation_id, message)
	continueSchema := toMap(t, toolList[1].Parameters)
	assert.Contains(t, props(continueSchema), "delegation_id")
	assert.Contains(t, props(continueSchema), "message")
	assert.ElementsMatch(t, requiredKeys(continueSchema), []any{"delegation_id", "message"})

	// Verify stop_delegation tool has correct schema (delegation_id)
	stopSchema := toMap(t, toolList[2].Parameters)
	assert.Contains(t, props(stopSchema), "delegation_id")
	assert.ElementsMatch(t, requiredKeys(stopSchema), []any{"delegation_id"})

	// Verify no descriptions contain the literal "description," prefix
	for _, tool := range toolList {
		schemaMap := toMap(t, tool.Parameters)
		buf, err := json.Marshal(schemaMap)
		require.NoError(t, err)
		assert.NotContains(t, string(buf), "description,",
			"tool %s schema contains literal 'description,' prefix", tool.Name)
	}
}

func TestDelegateToolInstructions(t *testing.T) {
	toolset := NewDelegateTool()
	instructions := toolset.Instructions()

	assert.Contains(t, instructions, "delegate")
	assert.Contains(t, instructions, "delegation_id")
	assert.Contains(t, instructions, "stop_delegation")
	assert.NotContains(t, instructions, "list_delegations")
	assert.NotContains(t, instructions, "view_delegation")
	assert.NotContains(t, instructions, "transfer_task")
	assert.NotContains(t, instructions, "run_background_agent")
}
