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
	require.Len(t, tools, 4)

	assert.Equal(t, ToolNameDelegate, tools[0].Name)
	assert.Equal(t, ToolNameListDelegations, tools[1].Name)
	assert.Equal(t, ToolNameViewDelegation, tools[2].Name)
	assert.Equal(t, ToolNameStopDelegation, tools[3].Name)
}

func TestDelegateToolInstructions(t *testing.T) {
	toolset := NewDelegateTool()
	instructions := toolset.Instructions()

	assert.Contains(t, instructions, "delegate")
	assert.Contains(t, instructions, "list_delegations")
	assert.Contains(t, instructions, "view_delegation")
	assert.Contains(t, instructions, "stop_delegation")
	assert.Contains(t, instructions, "transfer_task") // backward compat mention
	assert.Contains(t, instructions, "run_background_agent")
}

func TestDelegateModeConstants(t *testing.T) {
	assert.Equal(t, DelegateMode("async"), DelegateModeAsync)
	assert.Equal(t, DelegateMode("sync"), DelegateModeSync)
	assert.Equal(t, DelegateMode("handoff"), DelegateModeHandoff)
}
