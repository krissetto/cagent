package builtin

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDelegateTool(t *testing.T) {
	toolset := NewDelegateTool()
	tools, err := toolset.Tools(context.Background())
	require.NoError(t, err)
	require.Len(t, tools, 2)

	assert.Equal(t, ToolNameDelegate, tools[0].Name)
	assert.Equal(t, ToolNameStopDelegation, tools[1].Name)
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
