package delegation

import (
	"context"
	"errors"
	"regexp"
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

// waitForDelegation waits for a delegation's DoneCh to be closed with a timeout.
func waitForDelegation(t *testing.T, d *Delegation, timeout time.Duration) {
	t.Helper()
	select {
	case <-d.DoneCh:
	case <-time.After(timeout):
		t.Fatal("delegation did not complete within timeout")
	}
}

func TestGenerateShortID_Format(t *testing.T) {
	manager := NewManager(&MockRunner{})

	manager.mu.Lock()
	id := manager.generateShortID()
	manager.mu.Unlock()

	require.Len(t, id, shortIDLength)
	assert.Regexp(t, regexp.MustCompile(`^[a-z0-9]{5}$`), id)
}

func TestGenerateShortID_Uniqueness(t *testing.T) {
	manager := NewManager(&MockRunner{})
	seen := make(map[string]struct{}, 1000)

	manager.mu.Lock()
	defer manager.mu.Unlock()
	for range 1000 {
		id := manager.generateShortID()
		if _, exists := seen[id]; exists {
			t.Fatalf("duplicate short ID generated: %s", id)
		}
		seen[id] = struct{}{}
		manager.delegations[id] = &Delegation{ID: id}
	}
}

func TestManager_ShortIDLookup(t *testing.T) {
	runner := &MockRunner{result: "success"}
	manager := NewManager(runner)
	parentSession := session.New()

	delegationID, sessionID, err := manager.Start(context.Background(), StartParams{
		AgentName:       "agent1",
		Task:            "test task",
		ParentSessionID: parentSession.ID,
		ParentSession:   parentSession,
	})
	require.NoError(t, err)
	assert.NotEmpty(t, sessionID)
	assert.Regexp(t, regexp.MustCompile(`^[a-z0-9]{5}$`), delegationID)

	// Wait for the background goroutine to complete.
	d, ok := manager.Get(delegationID)
	require.True(t, ok)
	waitForDelegation(t, d, 5*time.Second)
	assert.Equal(t, "success", d.GetLastReply())
	assert.Equal(t, StatusCompleted, d.LoadStatus())

	// Continue: wait for DoneCh, then send a follow-up.
	runner.result = "continued"
	reply, err := manager.Continue(context.Background(), delegationID, "follow-up")
	require.NoError(t, err)
	assert.Equal(t, "continued", reply)
}

func TestManager_StopByShortID(t *testing.T) {
	// Use a delay so the goroutine is still running when we call Stop.
	runner := &MockRunner{result: "success", delay: 200 * time.Millisecond}
	manager := NewManager(runner)
	parentSession := session.New()

	delegationID, _, err := manager.Start(context.Background(), StartParams{
		AgentName:       "agent1",
		Task:            "test task",
		ParentSessionID: parentSession.ID,
		ParentSession:   parentSession,
	})
	require.NoError(t, err)

	d, ok := manager.Get(delegationID)
	require.True(t, ok)

	// Stop should succeed while the delegation is still running.
	err = manager.Stop(context.Background(), delegationID)
	require.NoError(t, err)

	// Wait for the goroutine to finish.
	waitForDelegation(t, d, 5*time.Second)
	assert.Equal(t, StatusCancelled, d.LoadStatus())
}

func TestDelegationContinue(t *testing.T) {
	runner := &MockRunner{result: "success"}

	manager := NewManager(runner)
	parentSession := session.New()

	// Start a delegation asynchronously.
	delegationID, _, err := manager.Start(context.Background(), StartParams{
		AgentName:       "agent1",
		Task:            "first task",
		ParentSessionID: parentSession.ID,
		ParentSession:   parentSession,
	})
	require.NoError(t, err)

	d, ok := manager.Get(delegationID)
	require.True(t, ok)
	// Wait for initial run before continuing.
	waitForDelegation(t, d, 5*time.Second)
	assert.Equal(t, "success", d.GetLastReply())

	// Continue the delegation with a new runner result.
	runner.result = "second response"
	reply, err := manager.Continue(context.Background(), delegationID, "follow-up message")
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

	// Wait for the delegation to complete before trying to stop it.
	d, ok := manager.Get(delegationID)
	require.True(t, ok)
	waitForDelegation(t, d, 5*time.Second)
	assert.Equal(t, StatusCompleted, d.LoadStatus())

	// Try to stop a completed delegation — should fail.
	err = manager.Stop(context.Background(), delegationID)
	assert.Error(t, err, "stopping a completed delegation should return an error")
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

	// Wait for initial run.
	d, ok := manager.Get(delegationID)
	require.True(t, ok)
	waitForDelegation(t, d, 5*time.Second)

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

	// Wait for background goroutine to complete.
	d, ok := manager.Get(delegationID)
	require.True(t, ok)
	waitForDelegation(t, d, 5*time.Second)

	// The runner's sessionReceived should be the child session.
	childSess := runner.sessionReceived
	require.NotNil(t, childSess)

	// Manually upsert it into the store to simulate successful initial run.
	err = store.UpdateSession(t.Context(), childSess)
	require.NoError(t, err)

	// Now continue and verify the stored session is loaded.
	runner.result = "continuation response"
	reply, err := manager.Continue(t.Context(), delegationID, "follow-up question")
	require.NoError(t, err)
	assert.Equal(t, "continuation response", reply)

	// The session passed to the second RunDelegation should have the same ID.
	contSess := runner.sessionReceived
	require.NotNil(t, contSess)
	assert.Equal(t, d.SessionID, contSess.ID)
}

// TestManager_DoneCh_RecreatedOnContinue verifies that DoneCh is recreated on
// continuation so callers waiting on it get a fresh signal.
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

	d, ok := manager.Get(delegationID)
	require.True(t, ok)

	// Wait for initial run to close DoneCh.
	waitForDelegation(t, d, 5*time.Second)
	select {
	case <-d.DoneCh:
		// Good, it was closed by Start goroutine
	default:
		t.Fatal("DoneCh should be closed after Start goroutine completes")
	}

	// Continue should recreate DoneCh so it can signal again.
	_, err = manager.Continue(t.Context(), delegationID, "more work")
	require.NoError(t, err)

	// DoneCh should be closed again after Continue completes.
	select {
	case <-d.DoneCh:
		// Good, it was closed by Continue
	default:
		t.Fatal("DoneCh should be closed after Continue")
	}
}

// TestManager_Start_ReturnsImmediately verifies that Start does not block on
// the child run — the goroutine runs asynchronously.
func TestManager_Start_ReturnsImmediately(t *testing.T) {
	const slowDelay = 200 * time.Millisecond
	runner := &MockRunner{result: "done", delay: slowDelay}
	manager := NewManager(runner)
	parentSession := session.New()

	start := time.Now()
	delegationID, sessionID, err := manager.Start(t.Context(), StartParams{
		AgentName:       "agent1",
		Task:            "slow task",
		ParentSessionID: parentSession.ID,
	})
	elapsed := time.Since(start)

	require.NoError(t, err)
	assert.NotEmpty(t, delegationID)
	assert.NotEmpty(t, sessionID)
	// Start should return in well under the delay (< half).
	assert.Less(t, elapsed, slowDelay/2,
		"Start should return immediately, not block on child run")

	// Wait for the goroutine to actually finish.
	d, ok := manager.Get(delegationID)
	require.True(t, ok)
	waitForDelegation(t, d, 5*time.Second)
}

// TestManager_ConcurrentStarts verifies that multiple Start calls can overlap
// without serialising on child completion.
func TestManager_ConcurrentStarts(t *testing.T) {
	const n = 5
	const delay = 100 * time.Millisecond

	// All runners sleep for `delay` before returning.
	runner := &MockRunner{result: "done", delay: delay}
	manager := NewManager(runner)
	parentSession := session.New()

	ids := make([]string, n)
	start := time.Now()
	for i := range n {
		id, _, err := manager.Start(t.Context(), StartParams{
			AgentName:       "agent1",
			Task:            "concurrent task",
			ParentSessionID: parentSession.ID,
		})
		require.NoError(t, err)
		ids[i] = id
	}
	// All n starts should have returned well before n*delay.
	require.Less(t, time.Since(start), time.Duration(n)*delay/2,
		"All Start calls should be non-blocking")

	// Wait for all goroutines.
	for _, id := range ids {
		d, ok := manager.Get(id)
		require.True(t, ok)
		waitForDelegation(t, d, 5*time.Second)
		assert.Equal(t, StatusCompleted, d.LoadStatus())
	}
}

// TestManager_StopAll_WaitsForGoroutines verifies that StopAll cancels running
// delegations and blocks until all background goroutines exit.
func TestManager_StopAll_WaitsForGoroutines(t *testing.T) {
	runner := &MockRunner{result: "done", delay: 500 * time.Millisecond}
	manager := NewManager(runner)

	ids := make([]string, 3)
	for i := range ids {
		id, _, err := manager.Start(t.Context(), StartParams{
			AgentName: "agent1",
			Task:      "task",
		})
		require.NoError(t, err)
		ids[i] = id
	}

	// Cancel all and wait.
	manager.StopAll()

	// After StopAll all goroutines must have finished.
	for _, id := range ids {
		d, ok := manager.Get(id)
		require.True(t, ok)
		select {
		case <-d.DoneCh:
			// closed — OK
		default:
			t.Errorf("delegation %s DoneCh should be closed after StopAll", id)
		}
		st := d.LoadStatus()
		assert.True(t, st == StatusCancelled || st == StatusCompleted,
			"unexpected status %s for delegation %s", st, id)
	}
}

// TestManager_OnCompletion_CalledOnSuccess verifies that the onCompletion
// callback fires with the reply when a delegation succeeds.
func TestManager_OnCompletion_CalledOnSuccess(t *testing.T) {
	runner := &MockRunner{result: "the answer"}
	var mu sync.Mutex
	var gotDelegation *Delegation
	var gotReply string
	var gotErr error
	called := make(chan struct{}, 1)

	manager := NewManager(runner, WithOnCompletion(func(d *Delegation, reply string, err error) {
		mu.Lock()
		gotDelegation = d
		gotReply = reply
		gotErr = err
		mu.Unlock()
		called <- struct{}{}
	}))

	delegationID, _, err := manager.Start(t.Context(), StartParams{
		AgentName: "agent1",
		Task:      "task",
	})
	require.NoError(t, err)

	select {
	case <-called:
	case <-time.After(5 * time.Second):
		t.Fatal("onCompletion was not called")
	}

	mu.Lock()
	defer mu.Unlock()
	require.NotNil(t, gotDelegation)
	assert.Equal(t, delegationID, gotDelegation.ID)
	assert.Equal(t, "the answer", gotReply)
	assert.NoError(t, gotErr)
}

// TestManager_OnCompletion_CalledOnFailure verifies that onCompletion fires
// with an error when the child run fails.
func TestManager_OnCompletion_CalledOnFailure(t *testing.T) {
	testErr := errors.New("child session exploded")
	runner := &MockRunner{err: testErr}
	called := make(chan struct{}, 1)
	var gotErr error

	manager := NewManager(runner, WithOnCompletion(func(d *Delegation, _ string, err error) {
		gotErr = err
		called <- struct{}{}
	}))

	_, _, err := manager.Start(t.Context(), StartParams{
		AgentName: "agent1",
		Task:      "task",
	})
	require.NoError(t, err)

	select {
	case <-called:
	case <-time.After(5 * time.Second):
		t.Fatal("onCompletion was not called on failure")
	}

	require.Error(t, gotErr)
	assert.Equal(t, testErr, gotErr)
}

// TestManager_BackgroundRunDoesNotSetBackgroundKey verifies that a Continue
// (synchronous) call does NOT tag the context with BackgroundRunKey —
// only the background goroutine launched by Start does.
func TestManager_BackgroundRunTagsContext(t *testing.T) {
	var ctxUsed context.Context
	captureRunner := &captureContextRunner{}
	manager := NewManager(captureRunner)

	_, _, err := manager.Start(t.Context(), StartParams{
		AgentName: "agent1",
		Task:      "task",
	})
	require.NoError(t, err)

	// Wait for goroutine.
	for range 100 {
		time.Sleep(time.Millisecond)
		captureRunner.mu.Lock()
		ctxUsed = captureRunner.lastCtx
		captureRunner.mu.Unlock()
		if ctxUsed != nil {
			break
		}
	}
	require.NotNil(t, ctxUsed, "runner was never called")
	assert.NotNil(t, ctxUsed.Value(BackgroundRunKey{}),
		"Start goroutine must tag context with BackgroundRunKey")
}

type captureContextRunner struct {
	mu      sync.Mutex
	lastCtx context.Context
}

func (r *captureContextRunner) RunDelegation(ctx context.Context, _ *Delegation, _ *session.Session) (string, error) {
	r.mu.Lock()
	r.lastCtx = ctx
	r.mu.Unlock()
	return "done", nil
}
