package runtime

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/runtime/delegation"
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

// --- Issue 1: DelegationStarted emitted BEFORE blocking sync Start ---

// TestHandleDelegate_SyncEmitsDelegationStartedBeforeBlock verifies that
// DelegationStartedEvent appears in the events channel before the sync
// delegation result is returned (i.e. while it is still running).
func TestHandleDelegate_SyncEmitsDelegationStartedBeforeBlock(t *testing.T) {
	rt, sess, evts := newDelegationTestRuntime(t)

	args, _ := json.Marshal(builtin.DelegateArgs{
		Agent: "worker",
		Task:  "crunch numbers",
		Mode:  builtin.DelegateModeSync,
	})
	toolCall := tools.ToolCall{
		ID:   "tc1",
		Type: "function",
		Function: tools.FunctionCall{
			Name:      builtin.ToolNameDelegate,
			Arguments: string(args),
		},
	}

	// handleDelegate must emit DelegationStarted BEFORE Start blocks.
	// We call it in a goroutine so we can observe whether the event arrives
	// while the call is still in flight.
	resultCh := make(chan *tools.ToolCallResult, 1)
	go func() {
		result, err := rt.handleDelegate(t.Context(), sess, toolCall, evts)
		if err != nil {
			resultCh <- nil
			return
		}
		resultCh <- result
	}()

	// Give the delegation a moment to start and emit events.
	time.Sleep(50 * time.Millisecond)

	// The DelegationStarted event should be in evts BEFORE handleDelegate returns.
	found := false
	for {
		select {
		case ev := <-evts:
			if _, ok := ev.(*DelegationStartedEvent); ok {
				found = true
			}
		default:
			goto done
		}
	}
done:
	// Now wait for the result.
	select {
	case <-resultCh:
	case <-time.After(5 * time.Second):
		t.Fatal("handleDelegate did not return in time")
	}

	assert.True(t, found, "DelegationStartedEvent must be emitted before sync delegation blocks")
}

// --- Issue 2: Async completion event sent to correct pinned channel ---

// TestHandleDelegate_AsyncCompletionUsePinnedChannel verifies that when an
// async delegation completes after the parent RunStream has ended (and the
// elicitationEventsChannel may be nil/stale), the completion event is still
// delivered to the channel that was active when the delegation was started.
func TestHandleDelegate_AsyncCompletionUsePinnedChannel(t *testing.T) {
	rt, sess, evts := newDelegationTestRuntime(t)

	args, _ := json.Marshal(builtin.DelegateArgs{
		Agent: "worker",
		Task:  "background job",
		Mode:  builtin.DelegateModeAsync,
	})
	toolCall := tools.ToolCall{
		ID:   "tc2",
		Type: "function",
		Function: tools.FunctionCall{
			Name:      builtin.ToolNameDelegate,
			Arguments: string(args),
		},
	}

	result, err := rt.handleDelegate(t.Context(), sess, toolCall, evts)
	require.NoError(t, err)
	require.False(t, result.IsError)

	// Simulate stale/nil channel by clearing the runtime-level channel.
	// This mimics the RunStream having returned while the async delegation
	// is still in flight.
	rt.swapElicitationEventsChannel(nil)

	// The async delegation should still complete and deliver its event via the
	// pinned per-delegation channel (d.Events), not via the runtime-level channel.
	var gotCompletion bool
	deadline := time.After(5 * time.Second)
	for !gotCompletion {
		select {
		case ev := <-evts:
			switch ev.(type) {
			case *DelegationCompletedEvent, *DelegationFailedEvent, *DelegationStoppedEvent:
				gotCompletion = true
			}
		case <-deadline:
			t.Fatal("timed out waiting for async delegation completion event on pinned channel")
		}
	}
}

// --- Issue 3: SubSessionCompletedEvent emitted after sync delegation ---

func TestHandleDelegate_SyncEmitsSubSessionCompleted(t *testing.T) {
	rt, sess, evts := newDelegationTestRuntime(t)

	args, _ := json.Marshal(builtin.DelegateArgs{
		Agent: "worker",
		Task:  "write a report",
		Mode:  builtin.DelegateModeSync,
	})
	toolCall := tools.ToolCall{
		ID:   "tc3",
		Type: "function",
		Function: tools.FunctionCall{
			Name:      builtin.ToolNameDelegate,
			Arguments: string(args),
		},
	}

	_, err := rt.handleDelegate(t.Context(), sess, toolCall, evts)
	require.NoError(t, err)

	// Collect all emitted events.
	collected := collectEvents(evts)

	var foundSubSess bool
	for _, ev := range collected {
		if _, ok := ev.(*SubSessionCompletedEvent); ok {
			foundSubSess = true
		}
	}
	assert.True(t, foundSubSess, "SubSessionCompletedEvent must be emitted after sync delegation")
}

func TestHandleTaskTransfer_EmitsSubSessionCompleted(t *testing.T) {
	rt, sess, evts := newDelegationTestRuntime(t)

	toolCall := tools.ToolCall{
		ID:   "tc4",
		Type: "function",
		Function: tools.FunctionCall{
			Name:      builtin.ToolNameTransferTask,
			Arguments: `{"agent":"worker","task":"analyse data","expected_output":"report"}`,
		},
	}

	_, err := rt.handleTaskTransfer(t.Context(), sess, toolCall, evts)
	require.NoError(t, err)

	collected := collectEvents(evts)

	var foundSubSess bool
	for _, ev := range collected {
		if _, ok := ev.(*SubSessionCompletedEvent); ok {
			foundSubSess = true
		}
	}
	assert.True(t, foundSubSess, "SubSessionCompletedEvent must be emitted after handleTaskTransfer")
}

// --- Issue 4: Empty-agent validation ---

func TestHandleDelegate_EmptyAgentReturnsError(t *testing.T) {
	rt, sess, evts := newDelegationTestRuntime(t)

	args, _ := json.Marshal(builtin.DelegateArgs{
		Agent: "",
		Task:  "do something",
	})
	toolCall := tools.ToolCall{
		ID:   "tc5",
		Type: "function",
		Function: tools.FunctionCall{
			Name:      builtin.ToolNameDelegate,
			Arguments: string(args),
		},
	}

	result, err := rt.handleDelegate(t.Context(), sess, toolCall, evts)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.True(t, result.IsError, "empty agent name should return error result")
	assert.Contains(t, result.Output, "agent name must not be empty")
}

func TestHandleDelegate_EmptyTaskReturnsError(t *testing.T) {
	rt, sess, evts := newDelegationTestRuntime(t)

	args, _ := json.Marshal(builtin.DelegateArgs{
		Agent: "worker",
		Task:  "",
	})
	toolCall := tools.ToolCall{
		ID:   "tc6",
		Type: "function",
		Function: tools.FunctionCall{
			Name:      builtin.ToolNameDelegate,
			Arguments: string(args),
		},
	}

	result, err := rt.handleDelegate(t.Context(), sess, toolCall, evts)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.True(t, result.IsError, "empty task should return error result")
	assert.Contains(t, result.Output, "task must not be empty")
}

// --- Issue 5: Nesting depth tracked via context ---

// TestHandleDelegate_ContextDelegationIDPropagated verifies that when a
// delegation runs, the context is annotated with the delegation ID so nested
// delegations can compute the correct parent delegation ID and depth.
func TestHandleDelegate_ContextDelegationIDPropagated(t *testing.T) {
	// Create a runtime where the worker runs another level of delegation.
	// We verify depth limiting by setting maxDepth=1 and using a runner
	// that captures the context key.
	var capturedParentID string

	runner := &captureParentIDRunner{
		capturedParentID: &capturedParentID,
		result:           &delegation.RunResult{Result: "ok"},
	}
	parentSession := session.New()
	m := delegation.NewManager(runner, delegation.WithMaxDepth(5))

	// Depth-1 delegation
	d1, err := m.Start(context.Background(), parentSession, "", "agent1", "task", "", delegation.ModeSyncDelegate)
	require.NoError(t, err)
	_ = d1

	// The runner captures the context's delegation ID; because the context is
	// injected with d1.ID in runSync, capturedParentID should equal d1.ID.
	assert.Equal(t, d1.ID, capturedParentID, "context should carry parent delegation ID for nested depth tracking")
}

// captureParentIDRunner reads the delegation ID from the context and stores it.
type captureParentIDRunner struct {
	capturedParentID *string
	result           *delegation.RunResult
}

func (r *captureParentIDRunner) RunDelegation(ctx context.Context, params delegation.RunParams) *delegation.RunResult {
	if v, ok := ctx.Value(delegation.ContextKeyDelegationID).(string); ok {
		*r.capturedParentID = v
	}
	return r.result
}

// --- Issue 8a: handleDelegate end-to-end through Manager.Start → RunDelegation ---

func TestHandleDelegate_AsyncEndToEnd(t *testing.T) {
	rt, sess, evts := newDelegationTestRuntime(t)

	args, _ := json.Marshal(builtin.DelegateArgs{
		Agent: "worker",
		Task:  "analyse logs",
		Mode:  builtin.DelegateModeAsync,
	})
	toolCall := tools.ToolCall{
		ID:   "tc7",
		Type: "function",
		Function: tools.FunctionCall{
			Name:      builtin.ToolNameDelegate,
			Arguments: string(args),
		},
	}

	result, err := rt.handleDelegate(t.Context(), sess, toolCall, evts)
	require.NoError(t, err)
	require.False(t, result.IsError, "async delegation should start successfully")
	assert.Contains(t, result.Output, "Delegation started with ID:")

	// Wait for completion via event.
	var completed bool
	deadline := time.After(5 * time.Second)
	for !completed {
		select {
		case ev := <-evts:
			switch ev.(type) {
			case *DelegationCompletedEvent, *DelegationFailedEvent:
				completed = true
			}
		case <-deadline:
			t.Fatal("async delegation completion event not received in time")
		}
	}
}

// --- Issue 8b: handleDelegate with mode=handoff ---

func TestHandleDelegate_HandoffMode(t *testing.T) {
	prov := &mockProvider{
		id:     "test/mock-model",
		stream: newStreamBuilder().AddContent("handoff done").AddStopWithUsage(5, 3).Build(),
	}
	worker := agent.New("worker", "Worker agent", agent.WithModel(prov))
	root := agent.New("root", "Root agent", agent.WithModel(prov),
		agent.WithSubAgents(worker),
	)
	// handoff requires handoffs list, not sub-agents
	agent.WithHandoffs(worker)(root)
	teamObj := team.New(team.WithAgents(root, worker))

	rt, err := NewLocalRuntime(teamObj, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	sess := session.New(session.WithUserMessage("Go"), session.WithToolsApproved(true))
	evts := make(chan Event, 256)

	args, _ := json.Marshal(builtin.DelegateArgs{
		Agent: "worker",
		Task:  "take over",
		Mode:  builtin.DelegateModeHandoff,
	})
	toolCall := tools.ToolCall{
		ID:   "tc8",
		Type: "function",
		Function: tools.FunctionCall{
			Name:      builtin.ToolNameDelegate,
			Arguments: string(args),
		},
	}

	result, err := rt.handleDelegate(t.Context(), sess, toolCall, evts)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.False(t, result.IsError, "handoff delegation should succeed")
	// After handoff, current agent should switch to worker.
	assert.Equal(t, "worker", rt.CurrentAgentName())
}

// --- Issue 8c: TestDelegationMaxConcurrent with a blocking runner ---

func TestDelegationMaxConcurrent_BlockingRunner(t *testing.T) {
	// A runner that blocks until unblocked.
	unblockAll := make(chan struct{})

	runner := &blockingRunner{
		unblockCh: unblockAll,
		result:    &delegation.RunResult{Result: "ok"},
	}

	m := delegation.NewManager(runner, delegation.WithMaxConcurrent(1))
	parentSession := session.New()

	// Start first async delegation — it will block inside RunDelegation.
	d1, err := m.Start(context.Background(), parentSession, "", "agent1", "task1", "", delegation.ModeAsyncDelegate)
	require.NoError(t, err)
	require.NotNil(t, d1)

	// Give runner time to enter the block.
	time.Sleep(30 * time.Millisecond)

	// Second async delegation should be rejected because maxConcurrent=1 is in use.
	_, err = m.Start(context.Background(), parentSession, "", "agent1", "task2", "", delegation.ModeAsyncDelegate)
	assert.Error(t, err, "second async delegation should be rejected when maxConcurrent=1 is in use")
	assert.Contains(t, err.Error(), "maximum concurrent delegations")

	// Unblock the first delegation so the test can exit cleanly.
	close(unblockAll)
	select {
	case <-d1.DoneCh:
	case <-time.After(2 * time.Second):
		t.Fatal("first delegation did not finish after unblock")
	}
}

// blockingRunner blocks RunDelegation until the unblock channel is closed.
type blockingRunner struct {
	unblockCh chan struct{}
	result    *delegation.RunResult
}

func (r *blockingRunner) RunDelegation(ctx context.Context, params delegation.RunParams) *delegation.RunResult {
	select {
	case <-r.unblockCh:
		return r.result
	case <-ctx.Done():
		return &delegation.RunResult{Stopped: true}
	}
}

// --- Issue 8d: handleTaskTransfer via delegation manager ---

func TestHandleTaskTransfer_EmitsDelegationStartedEvent(t *testing.T) {
	rt, sess, evts := newDelegationTestRuntime(t)

	toolCall := tools.ToolCall{
		ID:   "tc9",
		Type: "function",
		Function: tools.FunctionCall{
			Name:      builtin.ToolNameTransferTask,
			Arguments: `{"agent":"worker","task":"crunch numbers","expected_output":"result"}`,
		},
	}

	result, err := rt.handleTaskTransfer(t.Context(), sess, toolCall, evts)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.False(t, result.IsError, "task transfer should succeed")

	collected := collectEvents(evts)

	var foundStarted, foundTree bool
	for _, ev := range collected {
		switch ev.(type) {
		case *DelegationStartedEvent:
			foundStarted = true
		case *DelegationTreeEvent:
			foundTree = true
		}
	}
	assert.True(t, foundStarted, "DelegationStartedEvent must be emitted")
	assert.True(t, foundTree, "DelegationTreeEvent must be emitted")
}
