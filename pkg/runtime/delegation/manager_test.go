package delegation

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
)

// MockRunner implements DelegationRunner for testing
type MockRunner struct {
	CallCount int
	Results   map[string]*RunResult
}

func (m *MockRunner) RunDelegation(ctx context.Context, params RunParams) *RunResult {
	m.CallCount++
	if result, ok := m.Results[params.AgentName]; ok {
		return result
	}
	return &RunResult{Result: "default result"}
}

type blockingMockRunner struct {
	block   chan struct{}
	running *atomic.Int32
	result  *RunResult
}

func (m *blockingMockRunner) RunDelegation(ctx context.Context, params RunParams) *RunResult {
	m.running.Add(1)
	defer m.running.Add(-1)
	select {
	case <-m.block:
		return m.result
	case <-ctx.Done():
		return &RunResult{Stopped: true}
	}
}

func TestDelegationLifecycle(t *testing.T) {
	runner := &MockRunner{
		Results: map[string]*RunResult{
			"agent1": {Result: "success"},
		},
	}

	manager := NewManager(runner)
	parentSession := session.New()

	// Test delegation creation
	d, err := manager.Start(context.Background(), parentSession, "", "agent1", "test task", "expected output", ModeSyncDelegate)
	require.NoError(t, err)
	require.NotNil(t, d)
	assert.NotEmpty(t, d.ID)
	assert.Equal(t, "agent1", d.AgentName)
	assert.Equal(t, "test task", d.Task)

	// Test delegation was stored
	found, ok := manager.Get(d.ID)
	require.True(t, ok)
	assert.Equal(t, d.ID, found.ID)

	// Test delegation result
	assert.Equal(t, "success", d.Result)
	assert.Equal(t, StatusCompleted, d.LoadStatus())
}

func TestDelegationAsync(t *testing.T) {
	runner := &MockRunner{
		Results: map[string]*RunResult{
			"agent1": {Result: "async result"},
		},
	}

	manager := NewManager(runner)
	parentSession := session.New()

	d, err := manager.Start(context.Background(), parentSession, "", "agent1", "async task", "", ModeAsyncDelegate)
	require.NoError(t, err)
	require.NotNil(t, d)

	// Async should return immediately
	assert.Equal(t, ModeAsyncDelegate, d.Mode)

	// Wait for completion using DoneCh
	select {
	case <-d.DoneCh:
		assert.Equal(t, StatusCompleted, d.LoadStatus())
	case <-time.After(5 * time.Second):
		t.Fatal("delegation did not complete in time")
	}
}

func TestDelegationMaxConcurrent(t *testing.T) {
	var running atomic.Int32
	block := make(chan struct{})
	runner := &blockingMockRunner{
		block:   block,
		running: &running,
		result:  &RunResult{Result: "result"},
	}

	manager := NewManager(runner, WithMaxConcurrent(1))
	parentSession := session.New()

	// First async delegation should succeed and remain running.
	d1, err := manager.Start(context.Background(), parentSession, "", "agent1", "task1", "", ModeAsyncDelegate)
	require.NoError(t, err)
	require.NotNil(t, d1)

	require.Eventually(t, func() bool {
		return running.Load() == 1
	}, time.Second, 10*time.Millisecond)

	// Second async delegation should now be rejected while the first is still running.
	_, err = manager.Start(context.Background(), parentSession, "", "agent1", "task2", "", ModeAsyncDelegate)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "maximum concurrent delegations")

	close(block)
	select {
	case <-d1.DoneCh:
	case <-time.After(5 * time.Second):
		t.Fatal("delegation did not complete in time")
	}
}


func TestDelegationStop(t *testing.T) {
	runner := &MockRunner{
		Results: map[string]*RunResult{
			"agent1": {Result: "result"},
		},
	}

	manager := NewManager(runner)
	parentSession := session.New()

	d, err := manager.Start(context.Background(), parentSession, "", "agent1", "task", "", ModeAsyncDelegate)
	require.NoError(t, err)

	// Wait for it to start
	<-d.DoneCh

	// Now try to stop (should fail since it's already completed)
	err = manager.Stop(d.ID)
	assert.Error(t, err) // should be not running
}

func TestDelegationOutput(t *testing.T) {
	d := NewDelegation("parent-session", "", "agent1", "task", "output", ModeSyncDelegate)
	d.MaxOutput = 100

	// Append output
	d.AppendOutput("hello ")
	d.AppendOutput("world")

	output := d.GetOutput()
	assert.Equal(t, "hello world", output)

	// Test truncation
	d.AppendOutput(string(make([]byte, 200))) // Try to append 200 bytes
	output = d.GetOutput()
	assert.LessOrEqual(t, len(output), 100)
}

func TestDelegationTree(t *testing.T) {
	runner := &MockRunner{
		Results: map[string]*RunResult{
			"child1": {Result: "result1"},
			"child2": {Result: "result2"},
		},
	}

	manager := NewManager(runner)
	parentSession := session.New()

	// Create root delegation
	root, err := manager.Start(context.Background(), parentSession, "", "child1", "root task", "", ModeSyncDelegate)
	require.NoError(t, err)
	<-root.DoneCh

	// Create child delegation
	child, err := manager.Start(context.Background(), parentSession, root.ID, "child2", "child task", "", ModeSyncDelegate)
	require.NoError(t, err)
	<-child.DoneCh

	// Build tree
	tree := manager.Tree()
	require.Len(t, tree, 1)
	assert.Equal(t, root.ID, tree[0].ID)
	assert.Len(t, tree[0].Children, 1)
	assert.Equal(t, child.ID, tree[0].Children[0].ID)
}

func TestDelegationView(t *testing.T) {
	runner := &MockRunner{
		Results: map[string]*RunResult{
			"agent1": {Result: "test result"},
		},
	}

	manager := NewManager(runner)
	parentSession := session.New()

	d, err := manager.Start(context.Background(), parentSession, "", "agent1", "task", "", ModeSyncDelegate)
	require.NoError(t, err)
	<-d.DoneCh

	// View the delegation
	output, err := manager.View(d.ID)
	require.NoError(t, err)
	assert.Contains(t, output, "completed")
	assert.Contains(t, output, "test result")

	// View non-existent delegation
	_, err = manager.View("non-existent")
	assert.Error(t, err)
}

func TestDelegationList(t *testing.T) {
	runner := &MockRunner{
		Results: map[string]*RunResult{
			"agent1": {Result: "result"},
		},
	}

	manager := NewManager(runner)
	parentSession := session.New()

	d, err := manager.Start(context.Background(), parentSession, "", "agent1", "task", "", ModeSyncDelegate)
	require.NoError(t, err)
	<-d.DoneCh

	output := manager.List()
	assert.Contains(t, output, "Delegations:")
	assert.Contains(t, output, d.ID)
	assert.Contains(t, output, "completed")
}
