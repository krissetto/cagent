package subagent

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFindAllowed(t *testing.T) {
	t.Parallel()

	allowed := []AllowedSubagent{
		{Agent: "worker"},
		{Agent: "researcher", Name: "web", Description: "Web research"},
	}

	got, ok := FindAllowed(allowed, "worker")
	assert.True(t, ok)
	assert.Equal(t, "worker", got.Agent)

	got, ok = FindAllowed(allowed, "web")
	assert.True(t, ok, "alias resolves")
	assert.Equal(t, "researcher", got.Agent)

	got, ok = FindAllowed(allowed, "researcher")
	assert.True(t, ok, "underlying agent name resolves too")
	assert.Equal(t, "web", got.DisplayName())

	_, ok = FindAllowed(allowed, "stranger")
	assert.False(t, ok)

	_, ok = FindAllowed(nil, "anyone")
	assert.False(t, ok)
}

func TestInstructionsMentionCoreTools(t *testing.T) {
	t.Parallel()

	instr := Instructions()
	for _, name := range []string{ToolSpawnSubagent, ToolSendMessage, ToolReadSubagent} {
		assert.Contains(t, instr, name, "instructions mention %s", name)
	}
	assert.Contains(t, instr, "Do not poll")
	assert.Contains(t, instr, "To wait, first make sure work is running below you")
	assert.Contains(t, instr, "keep it conversational")
	assert.Contains(t, instr, "do not write\nbracketed status labels")
	assert.Contains(t, instr, "Your final response is\nreported automatically")
}
