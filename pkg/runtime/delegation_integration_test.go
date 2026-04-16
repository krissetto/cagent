package runtime

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

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
	assert.Contains(t, rt.toolMap, builtin.ToolNameContinueDelegation)
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
	startArgs, _ := json.Marshal(builtin.DelegateArgs{Agent: "worker", Task: "first task"})
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
	assert.Contains(t, result.Output, "delegation_id")
	assert.Contains(t, result.Output, "status")
	assert.Contains(t, result.Output, "started")

	// Extract delegation ID from result
	var parsed map[string]string
	err = json.Unmarshal([]byte(result.Output), &parsed)
	require.NoError(t, err)
	delegationID := parsed["delegation_id"]
	require.NotEmpty(t, delegationID)

	// Wait for the async goroutine to complete
	d, ok := rt.delegations.Get(delegationID)
	require.True(t, ok)
	select {
	case <-d.GetDoneCh():
		assert.Equal(t, "first reply", d.GetLastReply())
	case <-time.After(5 * time.Second):
		t.Fatal("delegation did not complete")
	}

	// Swap provider response for continuation
	prov.stream = newStreamBuilder().AddContent("second reply").AddStopWithUsage(10, 5).Build()

	contArgs, _ := json.Marshal(builtin.ContinueDelegationArgs{DelegationID: delegationID, Message: "follow-up task"})
	contToolCall := tools.ToolCall{
		ID:   "tc-cont",
		Type: "function",
		Function: tools.FunctionCall{
			Name:      builtin.ToolNameContinueDelegation,
			Arguments: string(contArgs),
		},
	}
	result2, err := rt.handleContinueDelegation(context.Background(), sess, contToolCall, evts)
	require.NoError(t, err)
	require.NotNil(t, result2)
	assert.False(t, result2.IsError)
	// continue_delegation is now async — result contains "message_sent", not the reply.
	assert.Contains(t, result2.Output, "message_sent")

	// Wait for the async continuation to produce the expected reply.
	// We poll because Continue() is async — the old DoneCh may already be
	// closed (from the initial Start run) before the new goroutine replaces it.
	childDelegation, ok := rt.delegations.Get(delegationID)
	require.True(t, ok)
	require.Eventually(t, func() bool {
		return childDelegation.GetLastReply() == "second reply"
	}, 5*time.Second, 10*time.Millisecond, "continuation did not produce expected reply")

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

// TestDelegate_NoDuplicateUserMessage verifies that RunDelegation does NOT
// reload or re-append user messages from the store. User messages should only
// be present once: when originally added by Manager.Start or Manager.Continue.
func TestDelegate_NoDuplicateUserMessage(t *testing.T) {
	prov := &mockProvider{
		id:     "test/mock-model",
		stream: newStreamBuilder().AddContent("first response").AddStopWithUsage(10, 5).Build(),
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

	// Start delegation with first task
	startArgs, _ := json.Marshal(builtin.DelegateArgs{Agent: "worker", Task: "first task"})
	startToolCall := tools.ToolCall{
		ID:   "tc-dup-1",
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

	var parsed map[string]string
	err = json.Unmarshal([]byte(result.Output), &parsed)
	require.NoError(t, err)
	delegationID := parsed["delegation_id"]
	require.NotEmpty(t, delegationID)

	// Wait for first delegation to complete
	d, ok := rt.delegations.Get(delegationID)
	require.True(t, ok)
	select {
	case <-d.GetDoneCh():
	case <-time.After(5 * time.Second):
		t.Fatal("first delegation did not complete")
	}

	assert.Equal(t, "first response", d.GetLastReply())

	// Change provider response for continuation
	prov.stream = newStreamBuilder().AddContent("second response").AddStopWithUsage(10, 5).Build()

	// Continue with follow-up message
	contArgs, _ := json.Marshal(builtin.ContinueDelegationArgs{
		DelegationID: delegationID,
		Message:      "follow-up",
	})
	contToolCall := tools.ToolCall{
		ID:   "tc-dup-2",
		Type: "function",
		Function: tools.FunctionCall{
			Name:      builtin.ToolNameContinueDelegation,
			Arguments: string(contArgs),
		},
	}
	result2, err := rt.handleContinueDelegation(context.Background(), sess, contToolCall, evts)
	require.NoError(t, err)
	require.NotNil(t, result2)
	assert.False(t, result2.IsError)
	assert.Contains(t, result2.Output, "message_sent")

	// Wait for the async continuation to finish. Poll because GetDoneCh() may
	// still return the already-closed Start DoneCh before the Continue goroutine
	// replaces it.
	childDelegation, ok := rt.delegations.Get(delegationID)
	require.True(t, ok)
	require.Eventually(t, func() bool {
		return childDelegation.GetLastReply() == "second response"
	}, 5*time.Second, 10*time.Millisecond, "continuation did not complete")

	// Load child session from store and verify no duplicate messages
	childSess, err := store.GetSession(context.Background(), childDelegation.SessionID)
	require.NoError(t, err)

	// Collect visible child-session user messages only (exclude parent-session
	// delegation notifications that may now be persisted asynchronously via the
	// steer queue on completion).
	var userMessages []string
	for _, item := range childSess.Messages {
		if item.Message != nil && item.Message.Message.Role == chat.MessageRoleUser && !item.Message.IsSubagentResult {
			userMessages = append(userMessages, item.Message.Message.Content)
		}
	}

	// Verify exactly 2 visible child-session user messages: first task and
	// follow-up, no duplicates.
	require.Len(t, userMessages, 2, "expected exactly 2 child user messages, got: %v", userMessages)
	assert.Equal(t, "first task", userMessages[0])
	assert.Equal(t, "follow-up", userMessages[1])
}
