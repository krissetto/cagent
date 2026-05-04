package supervisor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

// fakeLiveEventSource is a minimal runtime.LiveEventSource used by tests.
// It records the last context/session id it was called with and exposes
// the events channel to the test for direct pushing.
type fakeLiveEventSource struct {
	// events is returned from AttachLiveSession. Tests push events onto it.
	events chan runtime.Event
	// err, when non-nil, is returned from AttachLiveSession instead of events.
	err error

	gotCtx       context.Context //nolint:containedctx // test observability
	gotSessionID string
	callCount    int
}

func newFakeLiveEventSource() *fakeLiveEventSource {
	return &fakeLiveEventSource{events: make(chan runtime.Event, 16)}
}

func (f *fakeLiveEventSource) AttachLiveSession(ctx context.Context, sessionID string) (<-chan runtime.Event, error) {
	f.callCount++
	f.gotCtx = ctx
	f.gotSessionID = sessionID
	if f.err != nil {
		return nil, f.err
	}
	return f.events, nil
}

// newSupervisorForTest creates a supervisor with the programReady gate already
// closed so subscribe goroutines don't stall. No real *tea.Program is attached;
// runner state still updates because handleRuntimeEvent runs independently of
// program attachment.
func newSupervisorForTest() *Supervisor {
	s := New(nil)
	// Close the program-ready gate without setting an actual program.
	s.programReadyOnce.Do(func() { close(s.programReady) })
	return s
}

// waitFor polls cond until it returns true or the deadline elapses.
func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for: %s", msg)
}

// findTab returns the first tab matching "child-1" from the supervisor's
// locked GetTabs snapshot. Returns ok=false when not present. The id is
// hardcoded because every caller in this file uses the same fixture.
func findTab(s *Supervisor) (messages.TabInfo, bool) {
	const sessionID = "child-1"
	tabs, _ := s.GetTabs()
	for _, tab := range tabs {
		if tab.SessionID == sessionID {
			return tab, true
		}
	}
	return messages.TabInfo{}, false
}

func TestAttachSession_RegistersAttachedRunner(t *testing.T) {
	t.Parallel()
	s := newSupervisorForTest()
	src := newFakeLiveEventSource()

	node := runtime.LiveSessionNode{
		SessionID:       "child-1",
		ParentSessionID: "root-1",
		RootSessionID:   "root-1",
		AgentName:       "researcher",
		Title:           "Research child",
		Kind:            runtime.LiveSessionSubAgent,
	}

	runner, err := s.AttachSession(t.Context(), node, src)
	require.NoError(t, err)
	require.NotNil(t, runner)

	assert.Equal(t, "child-1", runner.ID)
	assert.Equal(t, "child-1", runner.SessionID)
	assert.Equal(t, SessionKindAttached, runner.Kind)
	assert.Equal(t, "root-1", runner.ParentSessionID)
	assert.Equal(t, "root-1", runner.RootSessionID)
	assert.Equal(t, "researcher", runner.AgentName)
	assert.Equal(t, "Research child", runner.Title)
	assert.Nil(t, runner.App, "attached runners have no *app.App")

	assert.Equal(t, 1, src.callCount, "source should be called exactly once")
	assert.Equal(t, "child-1", src.gotSessionID)

	// Tab info carries the new metadata.
	tabs, _ := s.GetTabs()
	require.Len(t, tabs, 1)
	assert.Equal(t, string(SessionKindAttached), tabs[0].Kind)
	assert.Equal(t, "root-1", tabs[0].ParentSessionID)
	assert.Equal(t, "root-1", tabs[0].RootSessionID)
	assert.Equal(t, "researcher", tabs[0].AgentName)
	assert.Equal(t, "Research child", tabs[0].Title)
}

func TestAttachSession_TitleFallbackToAgentName(t *testing.T) {
	t.Parallel()
	s := newSupervisorForTest()
	src := newFakeLiveEventSource()

	runner, err := s.AttachSession(t.Context(), runtime.LiveSessionNode{
		SessionID: "child-1",
		AgentName: "planner",
	}, src)
	require.NoError(t, err)
	assert.Equal(t, "planner", runner.Title)
}

func TestAttachSession_DedupesBySessionID(t *testing.T) {
	t.Parallel()
	s := newSupervisorForTest()
	src := newFakeLiveEventSource()

	node := runtime.LiveSessionNode{SessionID: "child-1", AgentName: "a"}

	first, err := s.AttachSession(t.Context(), node, src)
	require.NoError(t, err)

	second, err := s.AttachSession(t.Context(), node, src)
	require.NoError(t, err)

	assert.Same(t, first, second, "dedup should return the existing runner")
	assert.Equal(t, 1, src.callCount, "dedup should not open a second subscription")
	assert.Equal(t, 1, s.Count())
}

func TestAttachSession_DedupesAgainstOwnedRunner(t *testing.T) {
	t.Parallel()
	s := newSupervisorForTest()
	src := newFakeLiveEventSource()

	// Simulate an owned runner already existing for this ID. We insert it
	// directly because AddSession requires a full *app.App.
	owned := &SessionRunner{ID: "child-1", SessionID: "child-1", Kind: SessionKindOwned}
	s.runners[owned.ID] = owned
	s.order = append(s.order, owned.ID)

	runner, err := s.AttachSession(t.Context(), runtime.LiveSessionNode{SessionID: "child-1"}, src)
	require.NoError(t, err)

	assert.Same(t, owned, runner, "attach should not clobber an owned runner with the same id")
	assert.Equal(t, 0, src.callCount, "no subscription should be opened when deduping")
}

func TestAttachSession_ErrorsOnNilSource(t *testing.T) {
	t.Parallel()
	s := newSupervisorForTest()

	_, err := s.AttachSession(t.Context(), runtime.LiveSessionNode{SessionID: "c"}, nil)
	require.Error(t, err)
	assert.Equal(t, 0, s.Count())
}

func TestAttachSession_ErrorsOnEmptySessionID(t *testing.T) {
	t.Parallel()
	s := newSupervisorForTest()

	_, err := s.AttachSession(t.Context(), runtime.LiveSessionNode{}, newFakeLiveEventSource())
	require.Error(t, err)
	assert.Equal(t, 0, s.Count())
}

func TestAttachSession_SourceFailureDoesNotLeakRunner(t *testing.T) {
	t.Parallel()
	s := newSupervisorForTest()
	src := &fakeLiveEventSource{events: make(chan runtime.Event), err: errors.New("boom")}

	_, err := s.AttachSession(t.Context(), runtime.LiveSessionNode{SessionID: "c"}, src)
	require.Error(t, err)

	assert.Equal(t, 0, s.Count(), "failed attach should not leave a runner behind")
	assert.Empty(t, s.ActiveID())
}

func TestAttachSession_EventsUpdateRunnerStateAndPersistOrder(t *testing.T) {
	t.Parallel()
	s := newSupervisorForTest()
	src := newFakeLiveEventSource()

	_, err := s.AttachSession(t.Context(), runtime.LiveSessionNode{
		SessionID: "child-1", AgentName: "a",
	}, src)
	require.NoError(t, err)

	// StreamStartedEvent should flip the runner to running and update the tab.
	src.events <- runtime.StreamStarted("child-1", "a")

	waitFor(t, func() bool {
		tab, ok := findTab(s)
		return ok && tab.IsRunning
	}, "IsRunning true after StreamStartedEvent")

	// SessionTitleEvent should update the tab title.
	src.events <- runtime.SessionTitle("child-1", "Renamed")
	waitFor(t, func() bool {
		tab, ok := findTab(s)
		return ok && tab.Title == "Renamed"
	}, "Title updated after SessionTitleEvent")

	// StreamStoppedEvent clears the running flag.
	src.events <- runtime.StreamStopped("child-1", "a")
	waitFor(t, func() bool {
		tab, ok := findTab(s)
		return ok && !tab.IsRunning
	}, "IsRunning false after StreamStoppedEvent")
}

func TestCloseSession_AttachedDropsSubscriptionOnly(t *testing.T) {
	t.Parallel()
	s := newSupervisorForTest()
	src := newFakeLiveEventSource()

	runner, err := s.AttachSession(t.Context(), runtime.LiveSessionNode{
		SessionID: "child-1", AgentName: "a",
	}, src)
	require.NoError(t, err)
	require.NotNil(t, runner)

	// Confirm subscription is active by pushing an event and seeing state change.
	src.events <- runtime.StreamStarted("child-1", "a")
	waitFor(t, func() bool {
		tab, ok := findTab(s)
		return ok && tab.IsRunning
	}, "IsRunning true before close")

	// Closing should cancel the per-runner context (which our fake source sees
	// via the ctx it was given) and remove the runner.
	s.CloseSession("child-1")

	assert.Nil(t, s.GetRunner("child-1"))
	assert.Equal(t, 0, s.Count())

	// The context we were handed should now be Done.
	require.NotNil(t, src.gotCtx)
	select {
	case <-src.gotCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("source ctx was not cancelled after CloseSession")
	}
}

func TestCloseSession_OwnedDoesNotAffectAttachedSibling(t *testing.T) {
	t.Parallel()
	s := newSupervisorForTest()

	// Inject an owned-like runner without an App so we can exercise close.
	owned := &SessionRunner{ID: "root-1", SessionID: "root-1", Kind: SessionKindOwned}
	_, ownedCancel := context.WithCancel(t.Context())
	owned.cancel = ownedCancel
	s.runners[owned.ID] = owned
	s.order = append(s.order, owned.ID)
	s.activeID = owned.ID

	src := newFakeLiveEventSource()
	_, err := s.AttachSession(t.Context(), runtime.LiveSessionNode{
		SessionID: "child-1", ParentSessionID: "root-1", RootSessionID: "root-1",
	}, src)
	require.NoError(t, err)

	s.CloseSession("root-1")

	assert.Nil(t, s.GetRunner("root-1"))
	assert.NotNil(t, s.GetRunner("child-1"), "closing parent must not drop attached descendants")
	assert.Equal(t, 1, s.Count())

	// Attached subscription is still alive.
	select {
	case <-src.gotCtx.Done():
		t.Fatal("attached source ctx should remain live after parent close")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestSwitchTo_AcrossOwnedAndAttached(t *testing.T) {
	t.Parallel()
	s := newSupervisorForTest()

	// Owned-like runner (no App needed for the switch path).
	owned := &SessionRunner{ID: "root-1", SessionID: "root-1", Kind: SessionKindOwned, Title: "root"}
	s.runners[owned.ID] = owned
	s.order = append(s.order, owned.ID)
	s.activeID = owned.ID

	src := newFakeLiveEventSource()
	attached, err := s.AttachSession(t.Context(), runtime.LiveSessionNode{
		SessionID: "child-1", AgentName: "c",
	}, src)
	require.NoError(t, err)

	assert.Equal(t, "root-1", s.ActiveID())

	got := s.SwitchTo("child-1")
	require.NotNil(t, got)
	assert.Same(t, attached, got)
	assert.Equal(t, "child-1", s.ActiveID())

	got = s.SwitchTo("root-1")
	require.NotNil(t, got)
	assert.Same(t, owned, got)
	assert.Equal(t, "root-1", s.ActiveID())
}

func TestAttachSession_CancelledContextRemovesRunnerOnClose(t *testing.T) {
	t.Parallel()
	s := newSupervisorForTest()
	src := newFakeLiveEventSource()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	runner, err := s.AttachSession(ctx, runtime.LiveSessionNode{
		SessionID: "child-cancelled", AgentName: "a",
	}, src)
	require.NoError(t, err)
	require.NotNil(t, runner)

	// The source still saw the attach call, but its ctx is already cancelled.
	require.NotNil(t, src.gotCtx)
	select {
	case <-src.gotCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("expected attached source ctx to inherit cancellation")
	}

	// Closing the runner should still be well-behaved and leave no residue.
	next := s.CloseSession("child-cancelled")
	assert.Empty(t, next)
	assert.Equal(t, 0, s.Count())
	assert.Nil(t, s.GetRunner("child-cancelled"))
}

func TestAddSession_DedupesRepeatedSessionID(t *testing.T) {
	t.Parallel()
	// Use a supervisor with the default unopened programReady gate so the
	// AddSession subscription goroutine parks before touching the app. This lets
	// us exercise AddSession's internal dedupe invariant without needing a full
	// *app.App instance in this package.
	s := New(nil)

	sess := &session.Session{ID: "owned-1"}
	firstID := s.AddSession(t.Context(), nil, sess, "/tmp/one", nil)
	secondID := s.AddSession(t.Context(), nil, sess, "/tmp/two", nil)

	assert.Equal(t, "owned-1", firstID)
	assert.Equal(t, firstID, secondID, "repeat AddSession should keep the existing runner")
	assert.Equal(t, 1, s.Count(), "repeat AddSession must not duplicate runners")

	tabs, _ := s.GetTabs()
	require.Len(t, tabs, 1, "repeat AddSession must not duplicate tab order entries")
	assert.Equal(t, "owned-1", tabs[0].SessionID)
}
