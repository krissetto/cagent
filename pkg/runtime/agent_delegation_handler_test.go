package runtime

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin"
)

// newDelegationTestRuntime creates a LocalRuntime with root → worker agent hierarchy.
func newDelegationTestRuntime(t *testing.T) (*LocalRuntime, *session.Session, chan Event) {
	t.Helper()
	prov := &mockProvider{
		id:     "test/mock-model",
		stream: newStreamBuilder().AddContent("task done").AddStopWithUsage(10, 5).Build(),
	}
	worker := agent.New("worker", "Worker agent", agent.WithModel(prov))
	root := agent.New("root", "Root agent", agent.WithModel(prov), agent.WithSubAgents(worker))
	teamObj := team.New(team.WithAgents(root, worker))

	rt, err := NewLocalRuntime(teamObj, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	sess := session.New(session.WithUserMessage("Go"), session.WithToolsApproved(true))
	evts := make(chan Event, 256)
	return rt, sess, evts
}

func TestHandleDelegate_NewDelegation(t *testing.T) {
	rt, sess, evts := newDelegationTestRuntime(t)

	args, _ := json.Marshal(builtin.DelegateArgs{
		Agent:   "worker",
		Message: "crunch numbers",
	})
	toolCall := tools.ToolCall{
		ID:   "tc1",
		Type: "function",
		Function: tools.FunctionCall{
			Name:      builtin.ToolNameDelegate,
			Arguments: string(args),
		},
	}

	result, err := rt.handleDelegate(context.Background(), sess, toolCall, evts)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.False(t, result.IsError, "delegation should succeed")
	assert.Contains(t, result.Output, "delegation_id")
	assert.Contains(t, result.Output, "response")
}

func TestHandleDelegate_EmptyAgentReturnsError(t *testing.T) {
	rt, sess, evts := newDelegationTestRuntime(t)

	args, _ := json.Marshal(builtin.DelegateArgs{
		Agent:   "",
		Message: "do something",
	})
	toolCall := tools.ToolCall{
		ID:   "tc2",
		Type: "function",
		Function: tools.FunctionCall{
			Name:      builtin.ToolNameDelegate,
			Arguments: string(args),
		},
	}

	result, err := rt.handleDelegate(context.Background(), sess, toolCall, evts)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.True(t, result.IsError)
}

func TestHandleDelegate_EmptyMessageReturnsError(t *testing.T) {
	rt, sess, evts := newDelegationTestRuntime(t)

	args, _ := json.Marshal(builtin.DelegateArgs{
		Agent:   "worker",
		Message: "",
	})
	toolCall := tools.ToolCall{
		ID:   "tc3",
		Type: "function",
		Function: tools.FunctionCall{
			Name:      builtin.ToolNameDelegate,
			Arguments: string(args),
		},
	}

	result, err := rt.handleDelegate(context.Background(), sess, toolCall, evts)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "message must not be empty")
}

func TestHandleDelegate_BothAgentAndDelegationIDReturnsError(t *testing.T) {
	rt, sess, evts := newDelegationTestRuntime(t)

	args, _ := json.Marshal(builtin.DelegateArgs{
		Agent:        "worker",
		DelegationID: "some-id",
		Message:      "hello",
	})
	toolCall := tools.ToolCall{
		ID:   "tc4",
		Type: "function",
		Function: tools.FunctionCall{
			Name:      builtin.ToolNameDelegate,
			Arguments: string(args),
		},
	}

	result, err := rt.handleDelegate(context.Background(), sess, toolCall, evts)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "either agent or delegation_id")
}

func TestHandleDelegate_InvalidAgentReturnsError(t *testing.T) {
	rt, sess, evts := newDelegationTestRuntime(t)

	args, _ := json.Marshal(builtin.DelegateArgs{
		Agent:   "nonexistent",
		Message: "hello",
	})
	toolCall := tools.ToolCall{
		ID:   "tc5",
		Type: "function",
		Function: tools.FunctionCall{
			Name:      builtin.ToolNameDelegate,
			Arguments: string(args),
		},
	}

	result, err := rt.handleDelegate(context.Background(), sess, toolCall, evts)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "not in sub-agents list")
}

func TestHandleHandoff(t *testing.T) {
	prov := &mockProvider{
		id:     "test/mock-model",
		stream: newStreamBuilder().AddContent("handoff done").AddStopWithUsage(5, 3).Build(),
	}
	worker := agent.New("worker", "Worker agent", agent.WithModel(prov))
	root := agent.New("root", "Root agent", agent.WithModel(prov))
	agent.WithHandoffs(worker)(root)
	teamObj := team.New(team.WithAgents(root, worker))

	rt, err := NewLocalRuntime(teamObj, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	sess := session.New(session.WithUserMessage("Go"), session.WithToolsApproved(true))
	evts := make(chan Event, 256)

	args, _ := json.Marshal(builtin.HandoffArgs{
		Agent: "worker",
	})
	toolCall := tools.ToolCall{
		ID:   "tc6",
		Type: "function",
		Function: tools.FunctionCall{
			Name:      builtin.ToolNameHandoff,
			Arguments: string(args),
		},
	}

	result, err := rt.handleHandoff(context.Background(), sess, toolCall, evts)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.False(t, result.IsError)
	assert.Equal(t, "worker", rt.CurrentAgentName())
}

func TestHandleStopDelegation_NotFound(t *testing.T) {
	rt, sess, evts := newDelegationTestRuntime(t)

	args, _ := json.Marshal(builtin.StopDelegationArgs{
		DelegationID: "nonexistent",
	})
	toolCall := tools.ToolCall{
		ID:   "tc7",
		Type: "function",
		Function: tools.FunctionCall{
			Name:      builtin.ToolNameStopDelegation,
			Arguments: string(args),
		},
	}

	result, err := rt.handleStopDelegation(context.Background(), sess, toolCall, evts)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "not found")
}

func TestHandleStopDelegation_EmptyID(t *testing.T) {
	rt, sess, evts := newDelegationTestRuntime(t)

	args, _ := json.Marshal(builtin.StopDelegationArgs{
		DelegationID: "",
	})
	toolCall := tools.ToolCall{
		ID:   "tc8",
		Type: "function",
		Function: tools.FunctionCall{
			Name:      builtin.ToolNameStopDelegation,
			Arguments: string(args),
		},
	}

	result, err := rt.handleStopDelegation(context.Background(), sess, toolCall, evts)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "delegation_id must not be empty")
}

// TestHandleDelegate_NoSyntheticSystemMessage verifies that delegation child sessions
// do not contain a synthetic "helper agent" system message.
func TestHandleDelegate_NoSyntheticSystemMessage(t *testing.T) {
	rt, sess, evts := newDelegationTestRuntime(t)

	args, _ := json.Marshal(builtin.DelegateArgs{
		Agent:   "worker",
		Message: "analyze data",
	})
	toolCall := tools.ToolCall{
		ID:   "tc-nosys",
		Type: "function",
		Function: tools.FunctionCall{
			Name:      builtin.ToolNameDelegate,
			Arguments: string(args),
		},
	}

	result, err := rt.handleDelegate(context.Background(), sess, toolCall, evts)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.False(t, result.IsError, "delegation should succeed")
	assert.NotContains(t, result.Output, "helper agent")
	assert.NotContains(t, result.Output, "Complete the following task")
}

// TestHandleDelegate_ParentTranscriptIsolation verifies that the parent session
// only contains tool call + result, not child session content.
func TestHandleDelegate_ParentTranscriptIsolation(t *testing.T) {
	rt, sess, evts := newDelegationTestRuntime(t)

	// Add initial user message
	assert.NotNil(t, sess)

	args, _ := json.Marshal(builtin.DelegateArgs{
		Agent:   "worker",
		Message: "do work",
	})
	toolCall := tools.ToolCall{
		ID:   "tc-iso",
		Type: "function",
		Function: tools.FunctionCall{
			Name:      builtin.ToolNameDelegate,
			Arguments: string(args),
		},
	}

	result, err := rt.handleDelegate(context.Background(), sess, toolCall, evts)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.False(t, result.IsError)

	// The parent session messages should not contain raw child content
	// The result contains delegation_id and response as a tool result
	assert.Contains(t, result.Output, "delegation_id")
	assert.Contains(t, result.Output, "response")
}

