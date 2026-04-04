package runtime

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
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
	assert.Contains(t, rt.toolMap, builtin.ToolNameStopDelegation)
	assert.Contains(t, rt.toolMap, builtin.ToolNameHandoff)

	// Old tools should NOT be registered
	assert.NotContains(t, rt.toolMap, "transfer_task")
	assert.NotContains(t, rt.toolMap, "list_delegations")
	assert.NotContains(t, rt.toolMap, "view_delegation")
	assert.NotContains(t, rt.toolMap, "run_background_agent")
	assert.NotContains(t, rt.toolMap, "list_background_agents")
	assert.NotContains(t, rt.toolMap, "view_background_agent")
	assert.NotContains(t, rt.toolMap, "stop_background_agent")
}

// TestDelegation_MultiTurnConversation_EndToEnd verifies that a delegation can be
// started and then continued with the same child session preserving both turns.
func TestDelegation_MultiTurnConversation_EndToEnd(t *testing.T) {
	prov := &mockProvider{
		id:     "test/mock-model",
		stream: newStreamBuilder().AddContent("first reply").AddStopWithUsage(10, 5).Build(),
	}
	worker := agent.New("worker", "Worker", agent.WithModel(prov))
	root := agent.New("root", "Root", agent.WithModel(prov), agent.WithSubAgents(worker))
	teamObj := team.New(team.WithAgents(root, worker))

	store := session.NewInMemorySessionStore()
	rt, err := NewLocalRuntime(teamObj, WithSessionCompaction(false), WithModelStore(mockModelStore{}), WithSessionStore(store))
	require.NoError(t, err)

	sess := session.New(session.WithUserMessage("Go"), session.WithToolsApproved(true))
	err = store.AddSession(context.Background(), sess)
	require.NoError(t, err)
	evts := make(chan Event, 256)

	// Start delegation
	startArgs, _ := json.Marshal(builtin.DelegateArgs{Agent: "worker", Message: "first task"})
	startToolCall := tools.ToolCall{
		ID:   "tc-start",
		Type: "function",
		Function: tools.FunctionCall{
			Name:      builtin.ToolNameDelegate,
			Arguments: string(startArgs),
		},
	}
	result, err := rt.handleDelegate(context.Background(), sess, startToolCall, evts)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Output, "first reply")

	// Extract delegation ID from result
	var parsed map[string]string
	err = json.Unmarshal([]byte(result.Output), &parsed)
	require.NoError(t, err)
	delegationID := parsed["delegation_id"]
	require.NotEmpty(t, delegationID)

	// Swap provider response for continuation
	prov.stream = newStreamBuilder().AddContent("second reply").AddStopWithUsage(10, 5).Build()

	contArgs, _ := json.Marshal(builtin.DelegateArgs{DelegationID: delegationID, Message: "follow-up task"})
	contToolCall := tools.ToolCall{
		ID:   "tc-cont",
		Type: "function",
		Function: tools.FunctionCall{
			Name:      builtin.ToolNameDelegate,
			Arguments: string(contArgs),
		},
	}
	result2, err := rt.handleDelegate(context.Background(), sess, contToolCall, evts)
	require.NoError(t, err)
	require.NotNil(t, result2)
	assert.False(t, result2.IsError)
	assert.Contains(t, result2.Output, "second reply")

	childDelegation, ok := rt.delegations.Get(delegationID)
	require.True(t, ok)

	childSess, err := store.GetSession(context.Background(), childDelegation.SessionID)
	require.NoError(t, err)
	var userMessages []string
	for _, item := range childSess.Messages {
		if item.Message != nil && item.Message.Message.Role == chat.MessageRoleUser {
			userMessages = append(userMessages, item.Message.Message.Content)
		}
	}
	assert.Contains(t, userMessages, "first task")
	assert.Contains(t, userMessages, "follow-up task")
}
