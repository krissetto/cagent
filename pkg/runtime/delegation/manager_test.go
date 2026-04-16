package delegation

import (
	"context"
	"errors"
	"regexp"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
)

func TestManager_Start_PersistsInitialUserMessage(t *testing.T) {
	store := session.NewInMemorySessionStore()
	parentSess := session.New()
	require.NoError(t, store.AddSession(context.Background(), parentSess))

	runner := &MockRunner{result: "done"}
	mgr := NewManager(runner, WithSessionStore(store))

	delegationID, sessionID, err := mgr.Start(context.Background(), StartParams{
		AgentName:       "test-agent",
		Task:            "Fix the bug in main.go",
		ParentSessionID: parentSess.ID,
	})
	require.NoError(t, err)
	require.NotEmpty(t, delegationID)

	d, ok := mgr.Get(delegationID)
	require.True(t, ok)
	<-d.GetDoneCh()

	childSess, err := store.GetSession(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, childSess)

	var userMessages []string
	for _, item := range childSess.Messages {
		if item.Message != nil && item.Message.Message.Role == chat.MessageRoleUser && !item.Message.Implicit {
			userMessages = append(userMessages, item.Message.Message.Content)
		}
	}
	assert.Contains(t, userMessages, "Fix the bug in main.go",
		"Initial task should be persisted as a visible user message")
}

func TestManager_Continue_PersistsContinueMessage(t *testing.T) {
	store := session.NewInMemorySessionStore()
	parentSess := session.New()
	require.NoError(t, store.AddSession(context.Background(), parentSess))

	runner := &MockRunner{result: "initial done"}
	completions := make(chan struct{}, 10)
	mgr := NewManager(runner, WithSessionStore(store), WithOnCompletion(func(d *Delegation, reply string, err error) {
		completions <- struct{}{}
	}))

	delegationID, sessionID, err := mgr.Start(context.Background(), StartParams{
		AgentName:       "test-agent",
		Task:            "Initial task",
		ParentSessionID: parentSess.ID,
	})
	require.NoError(t, err)

	// Wait for initial Start completion
	select {
	case <-completions:
	case <-time.After(5 * time.Second):
		t.Fatal("initial delegation did not complete")
	}

	runner.result = "continue done"
	err = mgr.Continue(context.Background(), delegationID, "Please also fix the tests")
	require.NoError(t, err)

	// Wait for Continue completion
	select {
	case <-completions:
	case <-time.After(5 * time.Second):
		t.Fatal("continue did not complete")
	}

	childSess, err := store.GetSession(context.Background(), sessionID)
	require.NoError(t, err)

	var userMessages []string
	for _, item := range childSess.Messages {
		if item.Message != nil && item.Message.Message.Role == chat.MessageRoleUser && !item.Message.Implicit {
			userMessages = append(userMessages, item.Message.Message.Content)
		}
	}
	assert.Contains(t, userMessages, "Initial task",
		"Initial task should be persisted")
	assert.Contains(t, userMessages, "Please also fix the tests",
		"Continue message should be persisted")
}

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
	case <-d.GetDoneCh():
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
	completions := make(chan struct{}, 10)
	manager := NewManager(runner, WithOnCompletion(func(d *Delegation, reply string, err error) {
		completions <- struct{}{}
	}))
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

	// Wait for the initial Start completion.
	select {
	case <-completions:
	case <-time.After(5 * time.Second):
		t.Fatal("initial delegation did not complete")
	}

	d, ok := manager.Get(delegationID)
	require.True(t, ok)
	assert.Equal(t, "success", d.GetLastReply())
	assert.Equal(t, StatusCompleted, d.LoadStatus())

	// Continue and wait for completion.
	runner.result = "continued"
	err = manager.Continue(context.Background(), delegationID, "follow-up")
	require.NoError(t, err)
	select {
	case <-completions:
	case <-time.After(5 * time.Second):
		t.Fatal("continue did not complete")
	}
	assert.Equal(t, "continued", d.GetLastReply())
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
	completions := make(chan struct{}, 10)
	manager := NewManager(runner, WithOnCompletion(func(d *Delegation, reply string, err error) {
		completions <- struct{}{}
	}))
	parentSession := session.New()

	// Start a delegation asynchronously.
	delegationID, _, err := manager.Start(context.Background(), StartParams{
		AgentName:       "agent1",
		Task:            "first task",
		ParentSessionID: parentSession.ID,
		ParentSession:   parentSession,
	})
	require.NoError(t, err)

	// Wait for initial run completion.
	select {
	case <-completions:
	case <-time.After(5 * time.Second):
		t.Fatal("initial delegation did not complete")
	}

	d, ok := manager.Get(delegationID)
	require.True(t, ok)
	assert.Equal(t, "success", d.GetLastReply())

	// Continue the delegation with a new runner result.
	runner.result = "second response"
	err = manager.Continue(context.Background(), delegationID, "follow-up message")
	require.NoError(t, err)

	// Wait for Continue completion.
	select {
	case <-completions:
	case <-time.After(5 * time.Second):
		t.Fatal("continue did not complete")
	}

	assert.Equal(t, "second response", d.GetLastReply())
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

	err := manager.Continue(context.Background(), "nonexistent-id", "message")
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

	err = manager.Continue(context.Background(), delegationID, "")
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
	completions := make(chan struct{}, 10)

	manager := NewManager(runner, WithSessionStore(store), WithOnCompletion(func(d *Delegation, reply string, err error) {
		completions <- struct{}{}
	}))
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

	// Wait for initial completion.
	select {
	case <-completions:
	case <-time.After(5 * time.Second):
		t.Fatal("initial delegation did not complete")
	}

	d, ok := manager.Get(delegationID)
	require.True(t, ok)

	// The runner's sessionReceived should be the child session.
	// Manager.Start() persists it via UpdateSession after the run, so it's
	// already in the store without manual upserting.
	runner.mu.Lock()
	childSess := runner.sessionReceived
	runner.mu.Unlock()
	require.NotNil(t, childSess)

	// Now continue and verify the stored session is loaded.
	runner.result = "continuation response"
	err = manager.Continue(t.Context(), delegationID, "follow-up question")
	require.NoError(t, err)

	// Wait for Continue completion.
	select {
	case <-completions:
	case <-time.After(5 * time.Second):
		t.Fatal("continue did not complete")
	}

	assert.Equal(t, "continuation response", d.GetLastReply())

	// The session passed to the second RunDelegation should have the same ID.
	runner.mu.Lock()
	contSess := runner.sessionReceived
	runner.mu.Unlock()
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
	case <-d.GetDoneCh():
		// Good, it was closed by Start goroutine
	default:
		t.Fatal("DoneCh should be closed after Start goroutine completes")
	}

	// Continue should recreate DoneCh so it can signal again.
	err = manager.Continue(t.Context(), delegationID, "more work")
	require.NoError(t, err)

	// Wait for async Continue goroutine to complete.
	waitForDelegation(t, d, 5*time.Second)

	// DoneCh should be closed again after Continue goroutine completes.
	select {
	case <-d.GetDoneCh():
		// Good, it was closed by Continue goroutine
	default:
		t.Fatal("DoneCh should be closed after Continue goroutine completes")
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
		case <-d.GetDoneCh():
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

// blockingRunner allows controlling when RunDelegation returns via a channel.
// The first call returns immediately; subsequent calls block until released.
type blockingRunner struct {
	callCount int
	mu        sync.Mutex
	release   chan struct{}
}

func (r *blockingRunner) RunDelegation(ctx context.Context, _ *Delegation, _ *session.Session) (string, error) {
	r.mu.Lock()
	r.callCount++
	callNum := r.callCount
	r.mu.Unlock()

	// First call (Start) returns immediately
	if callNum == 1 {
		return "started", nil
	}

	// Subsequent calls (Continue) block until released or context is done
	select {
	case <-r.release:
		return "continued", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// funcRunner wraps a function as a DelegationRunner for testing.
type funcRunner struct {
	fn func(ctx context.Context, d *Delegation, sess *session.Session) (string, error)
}

func (r *funcRunner) RunDelegation(ctx context.Context, d *Delegation, sess *session.Session) (string, error) {
	return r.fn(ctx, d, sess)
}

// TestContinue_ConcurrentContinueQueues verifies that a second concurrent
// Continue() on the same delegation waits for the first to finish and then
// runs successfully (queue semantics, not rejection).
func TestContinue_ConcurrentContinueQueues(t *testing.T) {
	running1 := make(chan struct{})
	release1 := make(chan struct{})
	release2 := make(chan struct{})

	var callCount atomic.Int32
	runner := &funcRunner{fn: func(ctx context.Context, _ *Delegation, _ *session.Session) (string, error) {
		n := callCount.Add(1)
		switch n {
		case 1:
			return "initial", nil
		case 2:
			close(running1) // Signal that first Continue's runner is executing
			select {
			case <-release1:
				return "first-continue", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		default:
			select {
			case <-release2:
				return "second-continue", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
	}}

	completions := make(chan string, 10)
	manager := NewManager(runner, WithOnCompletion(func(d *Delegation, reply string, err error) {
		if err == nil {
			completions <- reply
		}
	}))

	delegationID, _, err := manager.Start(context.Background(), StartParams{
		AgentName: "agent1",
		Task:      "task",
	})
	require.NoError(t, err)

	// Wait for initial Start to complete.
	select {
	case r := <-completions:
		assert.Equal(t, "initial", r)
	case <-time.After(5 * time.Second):
		t.Fatal("initial delegation did not complete")
	}

	// Start first Continue (returns immediately).
	require.NoError(t, manager.Continue(context.Background(), delegationID, "msg1"))

	// Wait for first Continue's goroutine to reach the runner and signal.
	select {
	case <-running1:
	case <-time.After(5 * time.Second):
		t.Fatal("first Continue did not start")
	}

	// Now that first Continue is in the runner (and thus StatusRunning is set),
	// call second Continue. The second Continue's goroutine will see StatusRunning
	// and wait on the DoneCh from the first Continue.
	require.NoError(t, manager.Continue(context.Background(), delegationID, "msg2"))

	// Nothing completed yet (both runners are blocked).
	time.Sleep(50 * time.Millisecond)
	select {
	case r := <-completions:
		t.Fatalf("unexpected early completion: %s", r)
	default:
	}

	// Release first Continue.
	close(release1)
	select {
	case r := <-completions:
		assert.Equal(t, "first-continue", r)
	case <-time.After(5 * time.Second):
		t.Fatal("first continuation did not complete")
	}

	// Release second Continue.
	close(release2)
	select {
	case r := <-completions:
		assert.Equal(t, "second-continue", r)
	case <-time.After(5 * time.Second):
		t.Fatal("second continuation did not complete")
	}
}

func TestContinue_WaitsForInitialBackgroundRun(t *testing.T) {
	release := make(chan struct{})
	runner := &funcRunner{fn: func(ctx context.Context, _ *Delegation, _ *session.Session) (string, error) {
		select {
		case <-release:
			return "done", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}}

	completions := make(chan string, 10)
	manager := NewManager(runner, WithOnCompletion(func(d *Delegation, reply string, err error) {
		if err == nil {
			completions <- reply
		}
	}))

	delegationID, _, err := manager.Start(context.Background(), StartParams{
		AgentName: "agent1",
		Task:      "task",
	})
	require.NoError(t, err)

	// Continue returns immediately even though the initial run is still active.
	err = manager.Continue(context.Background(), delegationID, "follow up")
	require.NoError(t, err)

	// No completions yet (runner still blocked).
	select {
	case r := <-completions:
		t.Fatalf("unexpected early completion: %s", r)
	default:
	}

	// Release the runner (serves both initial and follow-up runs).
	close(release)

	// Expect two completions: initial + follow-up.
	for i := 0; i < 2; i++ {
		select {
		case r := <-completions:
			assert.Equal(t, "done", r)
		case <-time.After(5 * time.Second):
			t.Fatalf("completion %d did not arrive", i+1)
		}
	}
}

// TestContinue_DoneCh_ClosedExactlyOnce verifies that the DoneCh from a
// Continue run is closed exactly once after the run completes.
func TestContinue_DoneCh_ClosedExactlyOnce(t *testing.T) {
	runner := &MockRunner{result: "response"}

	completions := make(chan string, 10)
	manager := NewManager(runner, WithOnCompletion(func(d *Delegation, reply string, err error) {
		if err == nil {
			completions <- reply
		}
	}))

	delegationID, _, err := manager.Start(t.Context(), StartParams{
		AgentName: "agent1",
		Task:      "task",
	})
	require.NoError(t, err)

	// Wait for initial Start to complete.
	select {
	case <-completions:
	case <-time.After(5 * time.Second):
		t.Fatal("initial delegation did not complete")
	}

	// Continue — should not panic.
	runner.result = "continued"
	err = manager.Continue(t.Context(), delegationID, "msg")
	require.NoError(t, err)

	// Wait for the async Continue to complete.
	select {
	case r := <-completions:
		assert.Equal(t, "continued", r)
	case <-time.After(5 * time.Second):
		t.Fatal("Continue did not complete")
	}

	d, ok := manager.Get(delegationID)
	require.True(t, ok)
	assert.Equal(t, "continued", d.GetLastReply())

	// After Continue, DoneCh must be closed.
	select {
	case <-d.GetDoneCh():
		// OK: channel is closed
	default:
		t.Fatal("DoneCh should be closed after Continue completes")
	}
}

// TestContinue_WgTracked_BeforeVisible verifies that StopAll() called
// concurrently with a slow Continue() waits for the Continue to finish.
// With the new Continue() implementation StopAll also cancels the in-flight
// Continue via d.Cancel — so the invariant is: after StopAll returns,
// the Continue goroutine has fully exited (wg.Done was called).
func TestContinue_WgTracked_BeforeVisible(t *testing.T) {
	running := make(chan struct{})

	callCount := 0
	var mu sync.Mutex
	runner := &funcRunner{fn: func(ctx context.Context, _ *Delegation, _ *session.Session) (string, error) {
		mu.Lock()
		callCount++
		n := callCount
		mu.Unlock()

		if n == 1 {
			return "initial", nil
		}
		// Signal we're inside Continue's RunDelegation, then block until ctx done.
		close(running)
		<-ctx.Done()
		return "", ctx.Err()
	}}

	manager := NewManager(runner)
	delegationID, _, err := manager.Start(context.Background(), StartParams{
		AgentName: "agent1",
		Task:      "task",
	})
	require.NoError(t, err)

	d, ok := manager.Get(delegationID)
	require.True(t, ok)
	waitForDelegation(t, d, 5*time.Second)

	// Start slow Continue.
	continueDone := make(chan struct{})
	go func() {
		defer close(continueDone)
		_ = manager.Continue(context.Background(), delegationID, "msg")
	}()

	// Wait for Continue's RunDelegation to be executing.
	<-running

	// StopAll should cancel the in-flight Continue and block until it exits.
	manager.StopAll()

	// After StopAll returns, Continue must have finished.
	select {
	case <-continueDone:
		// Good — Continue goroutine has fully exited.
	default:
		t.Fatal("Continue goroutine still running after StopAll returned")
	}

	assert.Equal(t, StatusCancelled, d.LoadStatus())
}

// TestContinue_StopCancelsInFlightRun verifies that Stop() on a delegation
// that is in a slow Continue() causes the background goroutine to exit
// promptly with StatusCancelled.
func TestContinue_StopCancelsInFlightRun(t *testing.T) {
	running := make(chan struct{})
	callCount := 0
	var mu sync.Mutex
	runner := &funcRunner{fn: func(ctx context.Context, _ *Delegation, _ *session.Session) (string, error) {
		mu.Lock()
		callCount++
		n := callCount
		mu.Unlock()

		if n == 1 {
			return "initial", nil
		}
		// Signal that Continue's run is executing, then block.
		close(running)
		<-ctx.Done()
		return "", ctx.Err()
	}}

	completions := make(chan struct{}, 10)
	manager := NewManager(runner, WithOnCompletion(func(d *Delegation, reply string, err error) {
		completions <- struct{}{}
	}))
	delegationID, _, err := manager.Start(context.Background(), StartParams{
		AgentName: "agent1",
		Task:      "task",
	})
	require.NoError(t, err)

	// Wait for initial Start completion.
	select {
	case <-completions:
	case <-time.After(5 * time.Second):
		t.Fatal("initial delegation did not complete")
	}

	d, ok := manager.Get(delegationID)
	require.True(t, ok)

	// Start a slow Continue (returns immediately, goroutine runs in background).
	err = manager.Continue(context.Background(), delegationID, "msg")
	require.NoError(t, err)

	// Wait for Continue's runner to be executing.
	<-running

	// Stop the delegation while Continue is in-flight.
	require.NoError(t, manager.Stop(context.Background(), delegationID))

	// Wait for the Continue background goroutine to signal completion.
	select {
	case <-completions:
	case <-time.After(5 * time.Second):
		t.Fatal("Continue did not exit after Stop")
	}

	assert.Equal(t, StatusCancelled, d.LoadStatus())
}

// TestContinue_StopAllCancelsInFlightRun verifies that StopAll() cancels an
// in-flight Continue and blocks until it exits.
func TestContinue_StopAllCancelsInFlightRun(t *testing.T) {
	running := make(chan struct{})
	callCount := 0
	var mu sync.Mutex
	runner := &funcRunner{fn: func(ctx context.Context, _ *Delegation, _ *session.Session) (string, error) {
		mu.Lock()
		callCount++
		n := callCount
		mu.Unlock()

		if n == 1 {
			return "initial", nil
		}
		close(running)
		<-ctx.Done()
		return "", ctx.Err()
	}}

	manager := NewManager(runner)
	delegationID, _, err := manager.Start(context.Background(), StartParams{
		AgentName: "agent1",
		Task:      "task",
	})
	require.NoError(t, err)

	d, ok := manager.Get(delegationID)
	require.True(t, ok)
	waitForDelegation(t, d, 5*time.Second)

	// Start slow Continue.
	continueDone := make(chan struct{})
	go func() {
		defer close(continueDone)
		_ = manager.Continue(context.Background(), delegationID, "msg")
	}()

	<-running

	// StopAll should block until Continue exits.
	manager.StopAll()

	// After StopAll returns, Continue must have finished.
	select {
	case <-continueDone:
		// Good
	default:
		t.Fatal("Continue goroutine still running after StopAll returned")
	}

	assert.Equal(t, StatusCancelled, d.LoadStatus())
}

// TestManager_Evict_TerminalDelegation verifies that Evict removes a terminal delegation.
func TestManager_Evict_TerminalDelegation(t *testing.T) {
	runner := &MockRunner{result: "done"}
	manager := NewManager(runner)

	id, _, err := manager.Start(t.Context(), StartParams{AgentName: "a", Task: "t"})
	require.NoError(t, err)

	d, ok := manager.Get(id)
	require.True(t, ok)
	waitForDelegation(t, d, 5*time.Second)
	assert.Equal(t, StatusCompleted, d.LoadStatus())

	// Evict should succeed.
	assert.True(t, manager.Evict(id))

	// No longer findable.
	_, ok = manager.Get(id)
	assert.False(t, ok)

	// Second evict returns false.
	assert.False(t, manager.Evict(id))
}

// TestManager_Evict_RunningDelegationRejected verifies that Evict refuses to remove a running delegation.
func TestManager_Evict_RunningDelegationRejected(t *testing.T) {
	runner := &MockRunner{result: "done", delay: 500 * time.Millisecond}
	manager := NewManager(runner)

	id, _, err := manager.Start(t.Context(), StartParams{AgentName: "a", Task: "t"})
	require.NoError(t, err)

	// Should be running still.
	d, ok := manager.Get(id)
	require.True(t, ok)
	assert.Equal(t, StatusRunning, d.LoadStatus())

	// Evict must fail for running delegation.
	assert.False(t, manager.Evict(id))

	// Still findable.
	_, ok = manager.Get(id)
	assert.True(t, ok)

	manager.StopAll()
}

// TestManager_EvictionLoop_RemovesCancelledDelegation verifies that cancelled
// delegations record TerminatedAt and are auto-evicted after maxTerminalAge.
func TestManager_EvictionLoop_RemovesCancelledDelegation(t *testing.T) {
	runner := &MockRunner{result: "done", delay: 200 * time.Millisecond}
	manager := NewManager(runner, WithMaxTerminalAge(50*time.Millisecond))

	id, _, err := manager.Start(t.Context(), StartParams{AgentName: "a", Task: "t"})
	require.NoError(t, err)

	d, ok := manager.Get(id)
	require.True(t, ok)

	require.NoError(t, manager.Stop(t.Context(), id))
	waitForDelegation(t, d, 5*time.Second)
	assert.Equal(t, StatusCancelled, d.LoadStatus())
	assert.False(t, d.TerminatedAt.IsZero(), "cancelled delegation should record TerminatedAt")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager.StartEvictionLoop(ctx)

	time.Sleep(125 * time.Millisecond)

	_, ok = manager.Get(id)
	assert.False(t, ok, "cancelled delegation should have been evicted")
}

// TestManager_EvictionLoop_RemovesOldTerminal verifies the background eviction loop
// removes terminal delegations after maxTerminalAge.
func TestManager_EvictionLoop_RemovesOldTerminal(t *testing.T) {
	runner := &MockRunner{result: "done"}
	manager := NewManager(runner, WithMaxTerminalAge(50*time.Millisecond))

	id, _, err := manager.Start(t.Context(), StartParams{AgentName: "a", Task: "t"})
	require.NoError(t, err)

	d, ok := manager.Get(id)
	require.True(t, ok)
	waitForDelegation(t, d, 5*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager.StartEvictionLoop(ctx)

	// Wait for TTL + eviction interval to pass.
	time.Sleep(200 * time.Millisecond)

	_, ok = manager.Get(id)
	assert.False(t, ok, "terminal delegation should have been evicted")
}

// TestContinue_CallerCancellationCancelsRun verifies that cancelling the
// caller's context during the wait-for-running phase causes the Continue
// goroutine to drop out (the continue message is silently discarded).
// With async Continue, the caller's context is only used while waiting
// for a previous run to complete — not during the actual runner call.
func TestContinue_CallerCancellationCancelsRun(t *testing.T) {
	release := make(chan struct{})
	var callCount atomic.Int32
	runner := &funcRunner{fn: func(ctx context.Context, _ *Delegation, _ *session.Session) (string, error) {
		n := callCount.Add(1)
		if n == 1 {
			// Initial run blocks until released.
			select {
			case <-release:
				return "initial", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		return "continued", nil
	}}

	completions := make(chan string, 10)
	manager := NewManager(runner, WithOnCompletion(func(d *Delegation, reply string, err error) {
		if err == nil {
			completions <- reply
		}
	}))

	delegationID, _, err := manager.Start(context.Background(), StartParams{
		AgentName: "agent1",
		Task:      "task",
	})
	require.NoError(t, err)

	// The initial run is blocked. Call Continue with a cancellable context.
	// The Continue goroutine will wait for the initial run to complete.
	ctx, cancel := context.WithCancel(context.Background())
	err = manager.Continue(ctx, delegationID, "msg")
	require.NoError(t, err)

	// Give the Continue goroutine time to enter the wait-for-running select.
	time.Sleep(50 * time.Millisecond)

	// Cancel the caller's context — the Continue goroutine should drop out.
	cancel()

	// Give it time to exit.
	time.Sleep(50 * time.Millisecond)

	// Release the initial run so it completes normally.
	close(release)

	// Wait for the initial completion.
	select {
	case r := <-completions:
		assert.Equal(t, "initial", r)
	case <-time.After(5 * time.Second):
		t.Fatal("initial delegation did not complete")
	}

	d, ok := manager.Get(delegationID)
	require.True(t, ok)

	// The delegation completed normally; the Continue was silently dropped.
	assert.Equal(t, StatusCompleted, d.LoadStatus())
	assert.Equal(t, "initial", d.GetLastReply())

	// No second completion (Continue was dropped).
	select {
	case r := <-completions:
		t.Fatalf("unexpected second completion: %s — Continue should have been dropped", r)
	case <-time.After(200 * time.Millisecond):
		// Good — no second completion.
	}
}
