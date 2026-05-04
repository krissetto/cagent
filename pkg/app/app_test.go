package app

import (
	"context"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/sessiontitle"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin"
	mcptools "github.com/docker/docker-agent/pkg/tools/mcp"
)

// mockRuntime is a minimal mock for testing App without a real runtime
type mockRuntime struct{}

// busTestRuntime wraps mockRuntime and also implements
// [runtime.SessionObserverSubscriber] so [New] enables the bus path.
// Used by TestApp_ReplaceSession_ResubscribesBus.
type busTestRuntime struct {
	mockRuntime

	bus *runtime.EventBus
}

func (r *busTestRuntime) SubscribeSession(ctx context.Context, sessionID string, buffer int) *runtime.Subscription {
	return r.bus.Subscribe(ctx, sessionID, buffer)
}

type attachedTestRuntime struct {
	mockRuntime

	attachedEvents chan runtime.Event
	snapshot       runtime.StreamingSnapshot

	// mu guards the observability fields below. The attached-app flow
	// delivers Run() through a goroutine (see pkg/app/app.go: NewAttached.Run),
	// so tests racing the main goroutine against that background call must
	// synchronise every access to these fields or the race detector will
	// rightly complain.
	mu             sync.Mutex
	lastFollowUp   runtime.QueuedMessage
	lastSessionID  string
	interruptCount int
}

func (m *attachedTestRuntime) LastFollowUp() runtime.QueuedMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastFollowUp
}

func (m *attachedTestRuntime) LastSessionID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastSessionID
}

func (m *attachedTestRuntime) InterruptCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.interruptCount
}

func (m *attachedTestRuntime) AttachLiveSession(_ context.Context, _ string) (<-chan runtime.Event, error) {
	return m.attachedEvents, nil
}

func (m *attachedTestRuntime) AttachLiveSessionWithSnapshot(_ context.Context, _ string) (<-chan runtime.Event, runtime.StreamingSnapshot, error) {
	return m.attachedEvents, m.snapshot, nil
}

func (m *attachedTestRuntime) LiveSessionTree(rootSessionID string) []runtime.LiveSessionNode {
	return []runtime.LiveSessionNode{{SessionID: rootSessionID, RootSessionID: rootSessionID, AgentName: "worker"}}
}

func (m *attachedTestRuntime) LiveSessionNode(sessionID string) (runtime.LiveSessionNode, bool) {
	return runtime.LiveSessionNode{SessionID: sessionID, RootSessionID: "root", AgentName: "worker", Title: "Worker tab"}, true
}

func (m *attachedTestRuntime) SteerSessionByID(sessionID string, msg runtime.QueuedMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastSessionID = sessionID
	m.lastFollowUp = msg
	return nil
}

func (m *attachedTestRuntime) FollowUpSessionByID(sessionID string, msg runtime.QueuedMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastSessionID = sessionID
	m.lastFollowUp = msg
	return nil
}

func (m *attachedTestRuntime) CloseSessionByID(string) error { return nil }
func (m *attachedTestRuntime) StopSessionByID(string) error  { return nil }
func (m *attachedTestRuntime) InterruptSessionByID(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastSessionID = id
	m.interruptCount++
	return nil
}

type startupBlockingLiveRuntime struct {
	mockRuntime

	mu          sync.Mutex
	subscriber  chan runtime.Event
	latestTitle string

	startupStarted chan struct{}
	releaseStartup chan struct{}
}

func newStartupBlockingLiveRuntime() *startupBlockingLiveRuntime {
	return &startupBlockingLiveRuntime{
		startupStarted: make(chan struct{}),
		releaseStartup: make(chan struct{}),
	}
}

func (r *startupBlockingLiveRuntime) AttachLiveSession(_ context.Context, _ string) (<-chan runtime.Event, error) {
	ch := make(chan runtime.Event, 16)
	r.mu.Lock()
	r.subscriber = ch
	r.mu.Unlock()
	return ch, nil
}

func (r *startupBlockingLiveRuntime) EmitSessionStartupInfo(_ context.Context, _ *session.Session, _ string, _ chan runtime.Event) {
	select {
	case <-r.startupStarted:
		// already signalled
	default:
		close(r.startupStarted)
	}
	<-r.releaseStartup
}

func (r *startupBlockingLiveRuntime) Publish(ev runtime.Event) {
	r.mu.Lock()
	sub := r.subscriber
	r.mu.Unlock()
	if sub == nil || ev == nil {
		return
	}
	select {
	case sub <- ev:
	default:
	}
}

func (r *startupBlockingLiveRuntime) SetLatestTitle(title string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.latestTitle = title
}

func (r *startupBlockingLiveRuntime) LiveSessionTree(rootSessionID string) []runtime.LiveSessionNode {
	return []runtime.LiveSessionNode{{SessionID: rootSessionID, RootSessionID: rootSessionID, AgentName: "worker"}}
}

func (r *startupBlockingLiveRuntime) LiveSessionNode(sessionID string) (runtime.LiveSessionNode, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return runtime.LiveSessionNode{
		SessionID:     sessionID,
		RootSessionID: "root",
		AgentName:     "worker",
		Title:         r.latestTitle,
	}, true
}

func (r *startupBlockingLiveRuntime) SteerSessionByID(string, runtime.QueuedMessage) error {
	return nil
}

func (r *startupBlockingLiveRuntime) FollowUpSessionByID(string, runtime.QueuedMessage) error {
	return nil
}
func (r *startupBlockingLiveRuntime) CloseSessionByID(string) error     { return nil }
func (r *startupBlockingLiveRuntime) StopSessionByID(string) error      { return nil }
func (r *startupBlockingLiveRuntime) InterruptSessionByID(string) error { return nil }

func waitForAppEvent(t *testing.T, ch <-chan tea.Msg, pred func(tea.Msg) bool, label string) tea.Msg {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case msg := <-ch:
			if pred(msg) {
				return msg
			}
		case <-deadline:
			t.Fatalf("timeout waiting for %s", label)
		}
	}
}

func (m *mockRuntime) CurrentAgentInfo(ctx context.Context) runtime.CurrentAgentInfo {
	return runtime.CurrentAgentInfo{}
}
func (m *mockRuntime) CurrentAgentName() string          { return "mock" }
func (m *mockRuntime) SetCurrentAgent(name string) error { return nil }
func (m *mockRuntime) CurrentAgentTools(ctx context.Context) ([]tools.Tool, error) {
	return nil, nil
}

func (m *mockRuntime) EmitStartupInfo(ctx context.Context, sess *session.Session, events chan runtime.Event) {
}
func (m *mockRuntime) ResetStartupInfo() {}
func (m *mockRuntime) RunStream(ctx context.Context, sess *session.Session) <-chan runtime.Event {
	ch := make(chan runtime.Event)
	close(ch)
	return ch
}

func (m *mockRuntime) Run(ctx context.Context, sess *session.Session) ([]session.Message, error) {
	return nil, nil
}
func (m *mockRuntime) Resume(ctx context.Context, req runtime.ResumeRequest) {}
func (m *mockRuntime) ResumeElicitation(ctx context.Context, action tools.ElicitationAction, content map[string]any) error {
	return nil
}
func (m *mockRuntime) SessionStore() session.Store { return nil }
func (m *mockRuntime) Summarize(ctx context.Context, sess *session.Session, additionalPrompt string, events chan runtime.Event) {
}
func (m *mockRuntime) PermissionsInfo() *runtime.PermissionsInfo { return nil }
func (m *mockRuntime) CurrentAgentSkillsToolset() *builtin.SkillsToolset {
	return nil
}

func (m *mockRuntime) CurrentMCPPrompts(context.Context) map[string]mcptools.PromptInfo {
	return make(map[string]mcptools.PromptInfo)
}

func (m *mockRuntime) ExecuteMCPPrompt(context.Context, string, map[string]string) (string, error) {
	return "", nil
}

func (m *mockRuntime) UpdateSessionTitle(_ context.Context, sess *session.Session, title string) error {
	sess.Title = title
	return nil
}
func (m *mockRuntime) TitleGenerator() *sessiontitle.Generator { return nil }
func (m *mockRuntime) Close() error                            { return nil }
func (m *mockRuntime) Stop()                                   {}
func (m *mockRuntime) Steer(_ runtime.QueuedMessage) error     { return nil }
func (m *mockRuntime) FollowUp(_ runtime.QueuedMessage) error  { return nil }

// Verify mockRuntime implements runtime.Runtime
var _ runtime.Runtime = (*mockRuntime)(nil)

func TestApp_NewAttached_RoutesSendThroughFollowUpAndForwardsEvents(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	attachedCh := make(chan runtime.Event, 4)
	rt := &attachedTestRuntime{attachedEvents: attachedCh}
	sess := session.New(session.WithID("child-1"), session.WithAgentName("worker"))
	node := runtime.LiveSessionNode{SessionID: "child-1", AgentName: "worker", Title: "Worker tab"}

	attached := NewAttached(ctx, rt, sess, node)
	require.True(t, attached.attached, "NewAttached should set attached mode")
	require.NotNil(t, attached.attachedSend, "NewAttached should wire a send function when rt provides SessionTreeProvider")

	// First events: TeamInfo, AgentInfo, SessionTitle (for a non-empty node title).
	gotTypes := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for len(gotTypes) < 3 {
		select {
		case ev := <-attached.events:
			switch ev.(type) {
			case *runtime.TeamInfoEvent:
				gotTypes["team"] = true
			case *runtime.AgentInfoEvent:
				gotTypes["agent"] = true
			case *runtime.SessionTitleEvent:
				gotTypes["title"] = true
			}
		case <-deadline:
			t.Fatalf("timeout waiting for startup events; got %v", gotTypes)
		}
	}

	// Send a live event through the attached subscription and expect it forwarded.
	attachedCh <- runtime.Warning("hello from child", "worker")
	select {
	case ev := <-attached.events:
		warn, ok := ev.(*runtime.WarningEvent)
		require.True(t, ok, "expected the attached app to forward live warnings; got %T", ev)
		assert.Equal(t, "hello from child", warn.Message)
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for forwarded live event")
	}

	// Sending a user turn via Run should route through the SessionTreeProvider
	// rather than start a new RunStream or touch the child session directly.
	attached.Run(ctx, cancel, "do the thing", nil)
	deadline = time.After(2 * time.Second)
	for rt.LastFollowUp().Content == "" {
		select {
		case <-time.After(20 * time.Millisecond):
		case <-deadline:
			t.Fatal("timeout waiting for attached Run to call SessionTreeProvider")
		}
	}
	assert.Equal(t, "child-1", rt.LastSessionID())
	assert.Equal(t, "do the thing", rt.LastFollowUp().Content)
}

func TestApp_NewAttached_ReplaysStreamingSnapshotBeforeLiveEvents(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	attachedCh := make(chan runtime.Event, 4)
	rt := &attachedTestRuntime{
		attachedEvents: attachedCh,
		snapshot: runtime.StreamingSnapshot{
			AgentName: "worker",
			Content:   "partial response already streamed",
		},
	}
	sess := session.New(session.WithID("child-1"), session.WithAgentName("worker"))
	node := runtime.LiveSessionNode{SessionID: "child-1", AgentName: "worker", Title: "Worker tab"}

	attached := NewAttached(ctx, rt, sess, node)
	msg := waitForAppEvent(t, attached.events, func(msg tea.Msg) bool {
		ev, ok := msg.(*runtime.AgentChoiceEvent)
		return ok && ev.Content == "partial response already streamed"
	}, "attached streaming snapshot")

	choice, ok := msg.(*runtime.AgentChoiceEvent)
	require.True(t, ok)
	assert.Equal(t, "worker", choice.AgentName)
	assert.Equal(t, "child-1", choice.SessionID)
}

func TestApp_InterruptAttachedSessionRoutesToRuntime(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	rt := &attachedTestRuntime{attachedEvents: make(chan runtime.Event, 1)}
	sess := session.New(session.WithID("child-1"), session.WithAgentName("worker"))
	node := runtime.LiveSessionNode{SessionID: "child-1", AgentName: "worker", Title: "Worker tab"}

	attached := NewAttached(ctx, rt, sess, node)
	require.NoError(t, attached.InterruptAttachedSession())
	assert.Equal(t, "child-1", rt.LastSessionID())
	assert.Equal(t, 1, rt.InterruptCount())
}

func TestApp_InterruptAttachedSessionRejectsOwnedTabs(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	rt := &mockRuntime{}
	sess := session.New()
	app := New(ctx, rt, sess)

	err := app.InterruptAttachedSession()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "attached")
}

func TestApp_FollowUpQueuesThroughRuntime(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	rt := &attachedTestRuntime{}
	sess := session.New()
	app := New(ctx, rt, sess)

	require.NoError(t, app.FollowUp("keep going"))
	select {
	case <-time.After(50 * time.Millisecond):
	case <-ctx.Done():
	}
	// mockRuntime's FollowUp is embedded; it is a no-op. We're just asserting
	// that FollowUp does not panic and returns nil for a well-formed message.
}

func TestApp_ReplaceSession_ResubscribesBus(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	rt := &busTestRuntime{bus: runtime.NewEventBus()}
	initial := session.New(session.WithID("session-1"))
	app := New(ctx, rt, initial)
	require.True(t, app.busAvailable)

	replacement := session.New(session.WithID("session-2"))
	app.ReplaceSession(ctx, replacement)

	// Publish in a retry loop so the test does not depend on the
	// resubscribe goroutine winning the scheduling race against this
	// goroutine. The first publish that lands after the subscription
	// becomes active is forwarded onto app.events.
	publishCtx, stopPublishing := context.WithCancel(ctx)
	defer stopPublishing()
	go func() {
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-publishCtx.Done():
				return
			case <-ticker.C:
				rt.bus.Publish(replacement.ID, runtime.Warning("hello from replacement", "mock"))
			}
		}
	}()

	msg := waitForAppEvent(t, app.events, func(msg tea.Msg) bool {
		ev, ok := msg.(*runtime.WarningEvent)
		return ok && ev.Message == "hello from replacement"
	}, "replacement session bus event")

	warn, ok := msg.(*runtime.WarningEvent)
	require.True(t, ok)
	assert.Equal(t, "hello from replacement", warn.Message)
}

func TestApp_NewSession_PreservesToolsApproved(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	rt := &mockRuntime{}

	// Create initial session with tools approved
	initialSess := session.New(session.WithToolsApproved(true))
	require.True(t, initialSess.ToolsApproved, "Initial session should have tools approved")

	app := New(ctx, rt, initialSess)

	// Call NewSession - should preserve ToolsApproved
	app.NewSession()

	assert.True(t, app.Session().ToolsApproved, "NewSession should preserve ToolsApproved")
}

func TestApp_NewSession_PreservesHideToolResults(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	rt := &mockRuntime{}

	// Create initial session with hide tool results
	initialSess := session.New(session.WithHideToolResults(true))
	require.True(t, initialSess.HideToolResults, "Initial session should have HideToolResults")

	app := New(ctx, rt, initialSess)

	// Call NewSession - should preserve HideToolResults
	app.NewSession()

	assert.True(t, app.Session().HideToolResults, "NewSession should preserve HideToolResults")
}

func TestApp_NewSession_WithNilSession(t *testing.T) {
	t.Parallel()

	rt := &mockRuntime{}

	// Create app with nil session (edge case)
	app := &App{
		runtime: rt,
		session: nil,
	}

	// Call NewSession - should not panic and create a new session with defaults
	app.NewSession()

	require.NotNil(t, app.Session(), "NewSession should create a new session")
	assert.False(t, app.Session().ToolsApproved, "NewSession with nil should use default ToolsApproved=false")
}

func TestApp_UpdateSessionTitle(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	t.Run("updates title in session", func(t *testing.T) {
		t.Parallel()

		rt := &mockRuntime{}
		sess := session.New()
		events := make(chan tea.Msg, 16)
		app := &App{
			runtime: rt,
			session: sess,
			events:  events,
		}

		err := app.UpdateSessionTitle(ctx, "New Title")
		require.NoError(t, err)

		assert.Equal(t, "New Title", sess.Title)

		// Check that an event was emitted
		select {
		case event := <-events:
			titleEvent, ok := event.(*runtime.SessionTitleEvent)
			require.True(t, ok, "should emit SessionTitleEvent")
			assert.Equal(t, "New Title", titleEvent.Title)
		default:
			t.Fatal("expected SessionTitleEvent to be emitted")
		}
	})

	t.Run("returns error when no session", func(t *testing.T) {
		t.Parallel()

		rt := &mockRuntime{}
		app := &App{
			runtime: rt,
			session: nil,
		}

		err := app.UpdateSessionTitle(ctx, "New Title")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no active session")
	})

	t.Run("returns ErrTitleGenerating when generation in progress", func(t *testing.T) {
		t.Parallel()

		rt := &mockRuntime{}
		sess := session.New()
		events := make(chan tea.Msg, 16)
		app := &App{
			runtime: rt,
			session: sess,
			events:  events,
		}

		// Simulate title generation in progress
		app.titleGenerating.Store(true)

		err := app.UpdateSessionTitle(ctx, "New Title")
		require.ErrorIs(t, err, ErrTitleGenerating)

		// Title should not be updated
		assert.Empty(t, sess.Title)
	})
}

func TestApp_NewAttached_CatchesTitlePublishedDuringStartupGap(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	rt := newStartupBlockingLiveRuntime()
	sess := session.New(session.WithID("child-1"), session.WithAgentName("worker"))
	node := runtime.LiveSessionNode{SessionID: "child-1", AgentName: "worker"}

	attached := NewAttached(ctx, rt, sess, node)

	// Wait until the startup emitter is definitely blocking. In the old code,
	// the live subscription had not been attached yet at this point, so a
	// title event published now was lost forever.
	select {
	case <-rt.startupStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for startup emitter to block")
	}

	// Publish a live title update while startup is still blocked, and also
	// mirror it into the latest LiveSessionNode snapshot. The fixed code
	// should observe at least one of these paths (live subscription or
	// post-startup re-snapshot) and surface a SessionTitleEvent.
	rt.SetLatestTitle("Generated during startup")
	rt.Publish(runtime.SessionTitle(sess.ID, "Generated during startup"))
	close(rt.releaseStartup)

	msg := waitForAppEvent(t, attached.events, func(msg tea.Msg) bool {
		ev, ok := msg.(*runtime.SessionTitleEvent)
		return ok && ev.Title == "Generated during startup"
	}, "attached session title after startup gap")

	titleEv, ok := msg.(*runtime.SessionTitleEvent)
	require.True(t, ok)
	assert.Equal(t, "Generated during startup", titleEv.Title)
}

func TestApp_ResolveSkillCommand_NoLocalRuntime(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	rt := &mockRuntime{}
	sess := session.New()
	app := New(ctx, rt, sess)

	// mockRuntime is not a LocalRuntime, so no skills should be returned
	resolved, err := app.ResolveSkillCommand(ctx, "/some-skill")
	require.NoError(t, err)
	assert.Empty(t, resolved)
}

func TestApp_ResolveSkillCommand_NotSlashCommand(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	rt := &mockRuntime{}
	sess := session.New()
	app := New(ctx, rt, sess)

	resolved, err := app.ResolveSkillCommand(ctx, "not a slash command")
	require.NoError(t, err)
	assert.Empty(t, resolved)
}

func TestApp_RegenerateSessionTitle(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	t.Run("returns error when no session", func(t *testing.T) {
		t.Parallel()

		rt := &mockRuntime{}
		app := &App{
			runtime: rt,
			session: nil,
		}

		err := app.RegenerateSessionTitle(ctx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no active session")
	})

	t.Run("returns error when no title generator is available", func(t *testing.T) {
		t.Parallel()

		rt := &mockRuntime{}
		sess := session.New()
		events := make(chan tea.Msg, 16)
		app := &App{
			runtime: rt,
			session: sess,
			events:  events,
			// titleGen is nil - no title generator available
		}

		err := app.RegenerateSessionTitle(ctx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "title regeneration not available")
	})

	t.Run("returns ErrTitleGenerating when already generating", func(t *testing.T) {
		t.Parallel()

		rt := &mockRuntime{}
		sess := session.New()
		events := make(chan tea.Msg, 16)
		app := &App{
			runtime: rt,
			session: sess,
			events:  events,
		}

		// Simulate title generation already in progress
		app.titleGenerating.Store(true)

		err := app.RegenerateSessionTitle(ctx)
		require.ErrorIs(t, err, ErrTitleGenerating)
	})
}
