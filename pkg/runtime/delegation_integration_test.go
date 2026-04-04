package runtime

import (
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools/builtin"
)

// delegTestStream implements chat.MessageStream for delegation tests
type delegTestStream struct {
	responses []chat.MessageStreamResponse
	index     int
}

func (m *delegTestStream) Recv() (chat.MessageStreamResponse, error) {
	if m.index >= len(m.responses) {
		return chat.MessageStreamResponse{}, io.EOF
	}
	resp := m.responses[m.index]
	m.index++
	return resp, nil
}

func (m *delegTestStream) Close() {}

func TestRegisterDefaultToolsIncludesDelegation(t *testing.T) {
	stream := &delegTestStream{
		responses: []chat.MessageStreamResponse{
			{
				Choices: []chat.MessageStreamChoice{{
					Delta: chat.MessageDelta{Content: "done"},
				}},
			},
		},
	}
	provider := &mockProvider{id: "test/model", stream: stream}
	worker := agent.New("worker", "", agent.WithModel(provider))
	root := agent.New("root", "", agent.WithModel(provider), agent.WithSubAgents(worker))
	teamObj := team.New(team.WithAgents(root, worker))

	rt, err := NewLocalRuntime(teamObj)
	require.NoError(t, err)

	assert.Contains(t, rt.toolMap, builtin.ToolNameDelegate)
	assert.Contains(t, rt.toolMap, builtin.ToolNameListDelegations)
	assert.Contains(t, rt.toolMap, builtin.ToolNameViewDelegation)
	assert.Contains(t, rt.toolMap, builtin.ToolNameStopDelegation)
	assert.Contains(t, rt.toolMap, builtin.ToolNameTransferTask)
	assert.Contains(t, rt.toolMap, builtin.ToolNameHandoff)
	assert.Contains(t, rt.toolMap, "run_background_agent")
	assert.Contains(t, rt.toolMap, "list_background_agents")
	assert.Contains(t, rt.toolMap, "view_background_agent")
	assert.Contains(t, rt.toolMap, "stop_background_agent")
}
