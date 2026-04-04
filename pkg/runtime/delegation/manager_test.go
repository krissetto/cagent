package delegation

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
)

// MockRunner implements DelegationRunner for testing
type MockRunner struct {
	mu        sync.Mutex
	callCount int
	delay     time.Duration
	result    string
	err       error
	// sessionReceived stores the last session passed to RunDelegation
	sessionReceived *session.Session
}

func (m *MockRunner) RunDelegation(ctx context.Context, d *Delegation, sess *session.Session) (string, error) {
	m.mu.Lock()
	m.callCount++
	m.sessionReceived = sess
	m.mu.Unlock()

	if m.delay > 0 {
		select {
		case <-time.After(m.delay):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}

	return m.result, m.err
}

func TestDelegationLifecycle(t *testing.T) {
	runner := &MockRunner{result: "success"}

	manager := NewManager(runner)
	parentSession := session.New()

	// Test delegation creation
	delegationID, reply, err := manager.Start(context.Background(), StartParams{
		AgentName:       "agent1",
		Task:            "test task",
		ParentSessionID: parentSession.ID,
		ParentSession:   parentSession,
	})

	require.NoError(t, err)
	require.NotEmpty(t, delegationID)
	assert.Equal(t, "success", reply)

	// Test delegation was stored
	found, ok := manager.Get(delegationID)
	require.True(t, ok)
	assert.Equal(t, delegationID, found.SessionID)
	assert.Equal(t, "agent1", found.AgentName)
	assert.Equal(t, StatusCompleted, found.LoadStatus())
}

func TestDelegationContinue(t *testing.T) {
	runner := &MockRunner{result: "success"}

	manager := NewManager(runner)
	parentSession := session.New()

	// Start a delegation
	delegationID, reply, err := manager.Start(context.Background(), StartParams{
		AgentName:       "agent1",
		Task:            "first task",
		ParentSessionID: parentSession.ID,
		ParentSession:   parentSession,
	})

	require.NoError(t, err)
	assert.Equal(t, "success", reply)

	// Continue the delegation with a new runner result
	runner.result = "second response"
	reply, err = manager.Continue(context.Background(), delegationID, "follow-up message")
	require.NoError(t, err)
	assert.Equal(t, "second response", reply)
}

func TestDelegationStop(t *testing.T) {
	runner := &MockRunner{result: "result"}

	manager := NewManager(runner)
	parentSession := session.New()

	delegationID, _, err := manager.Start(context.Background(), StartParams{
		AgentName:       "agent1",
		Task:            "task",
		ParentSessionID: parentSession.ID,
		ParentSession:   parentSession,
	})
	require.NoError(t, err)

	// Try to stop a completed delegation
	err = manager.Stop(context.Background(), delegationID)
	assert.Error(t, err) // should be not running anymore
}

func TestDelegationEmptyAgentName(t *testing.T) {
	runner := &MockRunner{result: "result"}

	manager := NewManager(runner)
	parentSession := session.New()

	_, _, err := manager.Start(context.Background(), StartParams{
		AgentName:       "",
		Task:            "task",
		ParentSessionID: parentSession.ID,
		ParentSession:   parentSession,
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "agent name is required")
}

func TestDelegationEmptyTask(t *testing.T) {
	runner := &MockRunner{result: "result"}

	manager := NewManager(runner)
	parentSession := session.New()

	_, _, err := manager.Start(context.Background(), StartParams{
		AgentName:       "agent1",
		Task:            "",
		ParentSessionID: parentSession.ID,
		ParentSession:   parentSession,
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "message is required")
}

func TestDelegationContinueNotFound(t *testing.T) {
	runner := &MockRunner{result: "result"}

	manager := NewManager(runner)

	_, err := manager.Continue(context.Background(), "nonexistent-id", "message")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestDelegationContinueEmptyMessage(t *testing.T) {
	runner := &MockRunner{result: "result"}

	manager := NewManager(runner)
	parentSession := session.New()

	delegationID, _, err := manager.Start(context.Background(), StartParams{
		AgentName:       "agent1",
		Task:            "task",
		ParentSessionID: parentSession.ID,
		ParentSession:   parentSession,
	})
	require.NoError(t, err)

	_, err = manager.Continue(context.Background(), delegationID, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "message is required")
}

func TestDelegationStatus(t *testing.T) {
	d := NewDelegation("session1", "parent-session", "agent1")

	// Initial status should be pending
	assert.Equal(t, StatusPending, d.LoadStatus())

	// Update to running
	d.StoreStatus(StatusRunning)
	assert.Equal(t, StatusRunning, d.LoadStatus())

	// Update to completed
	d.StoreStatus(StatusCompleted)
	assert.Equal(t, StatusCompleted, d.LoadStatus())
}

func TestDelegationStatusString(t *testing.T) {
	tests := []struct {
		status DelegationStatus
		want   string
	}{
		{StatusPending, "pending"},
		{StatusRunning, "running"},
		{StatusCompleted, "completed"},
		{StatusFailed, "failed"},
		{StatusCancelled, "cancelled"},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.want, tt.status.String())
	}
}

func TestDelegationReply(t *testing.T) {
	d := NewDelegation("session1", "parent", "agent1")

	assert.Empty(t, d.GetLastReply())

	d.SetLastReply("hello world")
	assert.Equal(t, "hello world", d.GetLastReply())
}

func TestDelegationError(t *testing.T) {
	d := NewDelegation("session1", "parent", "agent1")

	assert.Nil(t, d.GetError())

	testErr := assert.AnError
	d.SetError(testErr)
	assert.Equal(t, testErr, d.GetError())
}

// TestManager_Continue_ReloadsExistingSession verifies that Continue loads the
// persisted child session from the store and appends the new message.
func TestManager_Continue_ReloadsExistingSession(t *testing.T) {
	store := session.NewInMemorySessionStore()
	runner := &MockRunner{result: "first response"}

	manager := NewManager(runner, WithSessionStore(store))
	parentSession := session.New()

	// Persist parent so it can be found
	err := store.AddSession(t.Context(), parentSession)
	require.NoError(t, err)

	// Start first delegation
	delegationID, _, err := manager.Start(t.Context(), StartParams{
		AgentName:       "agent1",
		Task:            "initial task",
		ParentSessionID: parentSession.ID,
		ParentSession:   parentSession,
	})
	require.NoError(t, err)
	require.NotEmpty(t, delegationID)

	// Persist the child session so Continue can reload it
	// (RunDelegation should have already done this, but for test we check if it works)
	childSess := runner.sessionReceived
	require.NotNil(t, childSess)

	// Manually upsert it into the store to simulate successful initial run
	err = store.UpdateSession(t.Context(), childSess)
	require.NoError(t, err)

	// Now continue and verify the stored session is loaded (runner gets the stored session)
	runner.result = "continuation response"
	reply, err := manager.Continue(t.Context(), delegationID, "follow-up question")
	require.NoError(t, err)
	assert.Equal(t, "continuation response", reply)

	// The session passed to the second RunDelegation should be the loaded one (same ID)
	contSess := runner.sessionReceived
	require.NotNil(t, contSess)
	assert.Equal(t, delegationID, contSess.ID)
}

// TestManager_DoneCh_RecreatedOnContinue verifies that DoneCh is recreated on continuation
// so callers waiting on it get a fresh signal.
func TestManager_DoneCh_RecreatedOnContinue(t *testing.T) {
	runner := &MockRunner{result: "response"}
	manager := NewManager(runner)
	parentSession := session.New()

	delegationID, _, err := manager.Start(t.Context(), StartParams{
		AgentName:       "agent1",
		Task:            "task",
		ParentSessionID: parentSession.ID,
	})
	require.NoError(t, err)

	// At this point DoneCh is closed (Start completed synchronously)
	d, ok := manager.Get(delegationID)
	require.True(t, ok)

	select {
	case <-d.DoneCh:
		// Good, it was closed by Start
	default:
		t.Fatal("DoneCh should be closed after Start")
	}

	// Continue should recreate DoneCh so it can signal again
	_, err = manager.Continue(t.Context(), delegationID, "more work")
	require.NoError(t, err)

	// DoneCh should be closed again after Continue completes
	select {
	case <-d.DoneCh:
		// Good, it was closed by Continue
	default:
		t.Fatal("DoneCh should be closed after Continue")
	}
}
