package tui

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/sessiontitle"
	"github.com/docker/docker-agent/pkg/tools"
	skillstool "github.com/docker/docker-agent/pkg/tools/builtin/skills"
	mcptools "github.com/docker/docker-agent/pkg/tools/mcp"
)

type attachBaseRuntime struct{}

func (attachBaseRuntime) CurrentAgentInfo(context.Context) runtime.CurrentAgentInfo {
	return runtime.CurrentAgentInfo{}
}

func (attachBaseRuntime) CurrentAgentName() string { return "root" }

func (attachBaseRuntime) SetCurrentAgent(string) error {
	return nil
}

func (attachBaseRuntime) CurrentAgentTools(context.Context) ([]tools.Tool, error) {
	return nil, nil
}

func (attachBaseRuntime) CurrentAgentToolsetStatuses() []tools.ToolsetStatus { return nil }
func (attachBaseRuntime) RestartToolset(context.Context, string) error       { return nil }

func (attachBaseRuntime) EmitStartupInfo(context.Context, *session.Session, runtime.EventSink) {
}
func (attachBaseRuntime) EmitAgentInfo(context.Context, runtime.EventSink) {}
func (attachBaseRuntime) ResetStartupInfo()                                {}

func (attachBaseRuntime) RunStream(context.Context, *session.Session) <-chan runtime.Event {
	ch := make(chan runtime.Event)
	close(ch)
	return ch
}

func (attachBaseRuntime) Run(context.Context, *session.Session) ([]session.Message, error) {
	return nil, nil
}

func (attachBaseRuntime) Resume(context.Context, runtime.ResumeRequest) {}

func (attachBaseRuntime) ResumeElicitation(context.Context, tools.ElicitationAction, map[string]any) error {
	return nil
}

func (attachBaseRuntime) SessionStore() session.Store { return nil }

func (attachBaseRuntime) Summarize(context.Context, *session.Session, string, runtime.EventSink) {
}

func (attachBaseRuntime) PermissionsInfo() *runtime.PermissionsInfo { return nil }

func (attachBaseRuntime) CurrentAgentSkillsToolset() *skillstool.ToolSet {
	return nil
}

func (attachBaseRuntime) RunSkillFork(context.Context, *session.Session, skillstool.RunSkillArgs, runtime.EventSink) (*tools.ToolCallResult, error) {
	return nil, nil
}

func (attachBaseRuntime) CurrentMCPPrompts(context.Context) map[string]mcptools.PromptInfo {
	return nil
}

func (attachBaseRuntime) ExecuteMCPPrompt(context.Context, string, map[string]string) (string, error) {
	return "", nil
}

func (attachBaseRuntime) UpdateSessionTitle(context.Context, *session.Session, string) error {
	return nil
}

func (attachBaseRuntime) TitleGenerator() *sessiontitle.Generator { return nil }
func (attachBaseRuntime) Steer(runtime.QueuedMessage) error       { return nil }
func (attachBaseRuntime) FollowUp(runtime.QueuedMessage) error    { return nil }

func (attachBaseRuntime) SetAgentModel(context.Context, string, string) error {
	return nil
}

func (attachBaseRuntime) CycleAgentThinkingLevel(context.Context, string) (effort.Level, error) {
	return "", runtime.ErrUnsupported
}

func (attachBaseRuntime) AvailableModels(context.Context) []runtime.ModelChoice { return nil }
func (attachBaseRuntime) SupportsModelSwitching() bool                          { return false }
func (attachBaseRuntime) OnToolsChanged(func(runtime.Event))                    {}
func (attachBaseRuntime) QueueStatus() runtime.QueueStatus                      { return runtime.QueueStatus{} }
func (attachBaseRuntime) TogglePause(context.Context) (bool, error)             { return false, nil }
func (attachBaseRuntime) Close() error                                          { return nil }

var _ runtime.Runtime = attachBaseRuntime{}

type attachHydrationRuntime struct {
	attachBaseRuntime

	store       session.Store
	live        *session.Session
	rootTitle   string
	childTitle  string
	followID    string
	steerID     string
	snapshot    []runtime.Event
	liveEvents  chan runtime.Event
	attachCalls int
}

func (r *attachHydrationRuntime) SessionStore() session.Store { return r.store }

func (r *attachHydrationRuntime) LiveChildSession(id string) (*session.Session, bool) {
	return r.live, r.live != nil && r.live.ID == id
}

func (r *attachHydrationRuntime) liveChildID() string {
	if r.live != nil {
		return r.live.ID
	}
	return "child"
}

func (r *attachHydrationRuntime) liveChildTitle() string {
	if r.childTitle != "" {
		return r.childTitle
	}
	if r.live != nil {
		return r.live.Title
	}
	return ""
}

func (r *attachHydrationRuntime) LiveSessionTree(_ context.Context, rootID string) (*runtime.LiveSessionTree, error) {
	children := []*runtime.LiveSessionNode{}
	if r.live != nil || r.childTitle != "" || (r.store == nil && rootID != "missing") {
		children = append(children, &runtime.LiveSessionNode{
			ID:        r.liveChildID(),
			ParentID:  "root",
			AgentName: "greppy",
			Title:     r.liveChildTitle(),
			Status:    "waiting",
			Live:      true,
		})
	}
	return &runtime.LiveSessionTree{
		Root: &runtime.LiveSessionNode{
			ID:       "root",
			Title:    r.rootTitle,
			Children: children,
		},
	}, nil
}

func (r *attachHydrationRuntime) AttachLiveSession(context.Context, string) (<-chan runtime.Event, func(), error) {
	ch := make(chan runtime.Event)
	close(ch)
	return ch, func() {}, nil
}

func (r *attachHydrationRuntime) AttachLiveSessionWithSnapshot(context.Context, string, int) ([]runtime.Event, <-chan runtime.Event, error) {
	r.attachCalls++
	ch := r.liveEvents
	if ch == nil {
		ch = make(chan runtime.Event)
		close(ch)
	}
	return r.snapshot, ch, nil
}

func (r *attachHydrationRuntime) FollowUpSessionByID(id string, msg runtime.QueuedMessage) error {
	r.followID = id
	return nil
}

func (r *attachHydrationRuntime) SteerSessionByID(id string, msg runtime.QueuedMessage) error {
	r.steerID = id
	return nil
}

func (r *attachHydrationRuntime) InterruptSessionByID(string) error { return nil }
func (r *attachHydrationRuntime) CloseSessionByID(string) error     { return nil }
func (r *attachHydrationRuntime) StopSessionByID(string) error      { return nil }

func TestHandleAttachSessionReloadedPersistedChildViewRendersHistoryBody(t *testing.T) {
	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	root := session.New(session.WithID("root"))
	root.WorkingDir = t.TempDir()
	reloadedRoot := root.Clone()
	reloadedRoot.AddSubSession(session.NewRuntimeManagedSubSession(reloadedRoot, session.WithID("child")))
	reloadedRoot.Messages[0].SubSession.AgentName = "greppy"
	reloadedRoot.Messages[0].SubSession.Title = "Child title"
	reloadedRoot.Messages[0].SubSession.AddMessage(session.UserMessage("persisted child prompt after reload"))
	reloadedRoot.Messages[0].SubSession.AddMessage(session.NewAgentMessage("greppy", &chat.Message{
		Role:    chat.MessageRoleAssistant,
		Content: "persisted child answer after reload",
	}))
	require.NoError(t, store.UpdateSession(ctx, reloadedRoot))

	rt := &attachHydrationRuntime{store: store}
	rootApp := app.New(ctx, rt, reloadedRoot)
	m := New(ctx, func(context.Context, string) (*app.App, *session.Session, func(), error) { return nil, nil, nil, nil }, rootApp, "", nil).(*appModel)
	resize := tea.WindowSizeMsg{Width: 220, Height: 40}
	model, cmd := m.Update(resize)
	m = model.(*appModel)
	if cmd != nil {
		_ = cmd()
	}
	model, cmd = m.handleAttachSession("child")
	m = model.(*appModel)
	if cmd != nil {
		if msg := cmd(); msg != nil {
			model, updateCmd := m.Update(msg)
			m = model.(*appModel)
			if updateCmd != nil {
				_ = updateCmd()
			}
		}
	}
	model, cmd = m.Update(resize)
	m = model.(*appModel)
	if cmd != nil {
		_ = cmd()
	}
	view := ansi.Strip(m.View().Content)
	require.Contains(t, view, "persisted child prompt after reload")
	require.Contains(t, view, "persisted child answer after reload")
}

func TestHandleAttachSessionLiveSwitchInitDedupesStoreAndSnapshotHistory(t *testing.T) {
	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	root := session.New(session.WithID("root"))
	require.NoError(t, store.AddSession(ctx, root))
	liveChild := session.NewRuntimeManagedSubSession(root, session.WithID("child"), session.WithAgentName("greppy"))
	storedChild := session.NewRuntimeManagedSubSession(root, session.WithID("child"), session.WithAgentName("greppy"))
	storedChild.AddMessage(session.UserMessage("dedupe live prompt"))
	storedChild.AddMessage(&session.Message{AgentName: "greppy", Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "dedupe live answer"}})
	require.NoError(t, store.AddSubSession(ctx, "root", storedChild))

	rt := &attachHydrationRuntime{store: store, live: liveChild, snapshot: []runtime.Event{
		runtime.UserMessage("dedupe live prompt", "child", nil, 0),
		runtime.AgentChoice("greppy", "child", "dedupe live answer"),
	}}
	rootApp := app.New(ctx, rt, root)
	m := New(ctx, func(context.Context, string) (*app.App, *session.Session, func(), error) { return nil, nil, nil, nil }, rootApp, "", nil).(*appModel)

	model, cmd := m.handleAttachSession("child")
	require.NotNil(t, cmd)
	m = model.(*appModel)
	if msg := cmd(); msg != nil {
		model, updateCmd := m.Update(msg)
		m = model.(*appModel)
		if updateCmd != nil {
			_ = updateCmd()
		}
	}

	m.chatPage.SetSize(120, 30)
	view := ansi.Strip(m.chatPage.View())
	require.Equal(t, 1, strings.Count(view, "dedupe live prompt"), view)
	require.Equal(t, 1, strings.Count(view, "dedupe live answer"), view)
	require.False(t, m.application.IsReadOnly())
	require.Equal(t, 1, rt.attachCalls)
}

func TestHandleAttachSessionHydratesEmptyLiveChildFromStoreAndKeepsChildQueue(t *testing.T) {
	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	root := session.New(session.WithID("root"))
	require.NoError(t, store.UpdateSession(ctx, root))
	liveChild := session.NewRuntimeManagedSubSession(root, session.WithID("child"))
	storedChild := session.NewRuntimeManagedSubSession(root, session.WithID("child"))
	storedChild.AddMessage(session.UserMessage("delegated prompt"))
	require.NoError(t, store.AddSubSession(ctx, "root", storedChild))

	rt := &attachHydrationRuntime{store: store, live: liveChild}
	rootApp := app.New(ctx, rt, root)
	m := New(ctx, func(context.Context, string) (*app.App, *session.Session, func(), error) { return nil, nil, nil, nil }, rootApp, "", nil).(*appModel)

	model, cmd := m.handleAttachSession("child")
	require.NotNil(t, cmd)
	m = model.(*appModel)
	require.Equal(t, "child", m.application.Session().ID)
	require.Len(t, m.application.Session().Messages, 1)
	require.Contains(t, m.application.Session().Messages[0].Message.Message.Content, "delegated prompt")

	require.NoError(t, m.application.FollowUpWithAttachments("hello child", nil))
	require.Equal(t, "child", rt.followID)
}

func TestHandleAttachSessionHydratesStoredChildAndInitialChatPageRendersHistory(t *testing.T) {
	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	root := session.New(session.WithID("root"))
	require.NoError(t, store.AddSession(ctx, root))
	liveChild := session.NewRuntimeManagedSubSession(root, session.WithID("child"))
	storedChild := session.NewRuntimeManagedSubSession(root, session.WithID("child"))
	storedChild.AddMessage(session.UserMessage("delegated prompt"))
	storedChild.AddMessage(&session.Message{AgentName: "greppy", Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "persisted child reply"}})
	require.NoError(t, store.AddSubSession(ctx, "root", storedChild))

	rt := &attachHydrationRuntime{store: store, live: liveChild}
	rootApp := app.New(ctx, rt, root)
	m := New(ctx, func(context.Context, string) (*app.App, *session.Session, func(), error) { return nil, nil, nil, nil }, rootApp, "", nil).(*appModel)

	model, cmd := m.handleAttachSession("child")
	require.NotNil(t, cmd)
	m = model.(*appModel)
	require.Equal(t, "child", m.application.Session().ID)
	require.Len(t, m.application.Session().Messages, 2)

	if initCmd := m.chatPage.Init(); initCmd != nil {
		_ = initCmd()
	}
	m.chatPage.SetSize(100, 24)
	view := m.chatPage.View()
	require.Contains(t, view, "delegated prompt")
	require.Contains(t, view, "persisted child reply")

	require.NoError(t, m.application.FollowUpWithAttachments("hello child", nil))
	require.Equal(t, "child", rt.followID)
}

func TestHandleAttachSessionStoreWinsWhenLiveHasSameCountButPartialContent(t *testing.T) {
	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	root := session.New(session.WithID("root"))
	require.NoError(t, store.AddSession(ctx, root))
	liveChild := session.NewRuntimeManagedSubSession(root, session.WithID("child"))
	liveChild.AddMessage(session.UserMessage(""))
	storedChild := session.NewRuntimeManagedSubSession(root, session.WithID("child"))
	storedChild.AddMessage(session.UserMessage("visible stored content"))
	require.NoError(t, store.AddSubSession(ctx, "root", storedChild))

	rt := &attachHydrationRuntime{store: store, live: liveChild}
	rootApp := app.New(ctx, rt, root)
	m := New(ctx, func(context.Context, string) (*app.App, *session.Session, func(), error) { return nil, nil, nil, nil }, rootApp, "", nil).(*appModel)

	model, cmd := m.handleAttachSession("child")
	require.NotNil(t, cmd)
	m = model.(*appModel)
	require.Equal(t, "child", m.application.Session().ID)
	require.Len(t, m.application.Session().Messages, 1)
	require.Equal(t, "visible stored content", m.application.Session().Messages[0].Message.Message.Content)
}

func TestHandleAttachSessionFallsBackToLiveMessagesWhenStoreMissing(t *testing.T) {
	ctx := t.Context()
	root := session.New(session.WithID("root"))
	liveChild := session.NewRuntimeManagedSubSession(root, session.WithID("child"))
	liveChild.AddMessage(session.UserMessage("live fallback content"))

	rt := &attachHydrationRuntime{live: liveChild}
	rootApp := app.New(ctx, rt, root)
	m := New(ctx, func(context.Context, string) (*app.App, *session.Session, func(), error) { return nil, nil, nil, nil }, rootApp, "", nil).(*appModel)

	model, cmd := m.handleAttachSession("child")
	require.NotNil(t, cmd)
	m = model.(*appModel)
	require.Equal(t, "child", m.application.Session().ID)
	require.Len(t, m.application.Session().Messages, 1)
	require.Equal(t, "live fallback content", m.application.Session().Messages[0].Message.Message.Content)
}

func TestHandleAttachSessionOpensEmptyLiveStateWhenNoHistory(t *testing.T) {
	ctx := t.Context()
	root := session.New(session.WithID("root"))
	rt := &attachHydrationRuntime{}
	rootApp := app.New(ctx, rt, root)
	m := New(ctx, func(context.Context, string) (*app.App, *session.Session, func(), error) { return nil, nil, nil, nil }, rootApp, "", nil).(*appModel)

	model, cmd := m.handleAttachSession("child")
	require.NotNil(t, cmd)
	m = model.(*appModel)
	require.Equal(t, "child", m.application.Session().ID)
	require.Empty(t, m.application.Session().Messages)
	require.Equal(t, 1, rt.attachCalls)
}

func TestHandleAttachSessionOpensNonLiveStoredChildHistoryOnlyReadOnlyWithTitle(t *testing.T) {
	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	root := session.New(session.WithID("root"))
	require.NoError(t, store.AddSession(ctx, root))
	storedChild := session.NewRuntimeManagedSubSession(root, session.WithID("child"), session.WithTitle("Stored Child Title"))
	storedChild.AddMessage(session.UserMessage("stored historical prompt"))
	require.NoError(t, store.AddSubSession(ctx, "root", storedChild))

	rt := &attachHydrationRuntime{store: store, rootTitle: "Root Title"}
	rootApp := app.New(ctx, rt, root)
	m := New(ctx, func(context.Context, string) (*app.App, *session.Session, func(), error) { return nil, nil, nil, nil }, rootApp, "", nil).(*appModel)

	model, cmd := m.handleAttachSession("child")
	require.NotNil(t, cmd)
	m = model.(*appModel)
	require.Equal(t, "child", m.application.Session().ID)
	require.Equal(t, "Stored Child Title", m.application.Session().Title)
	require.Equal(t, "Stored Child Title", m.sessionState.SessionTitle())
	require.True(t, m.application.IsReadOnly())
	require.ErrorContains(t, m.application.FollowUpWithAttachments("nope", nil), "follow-up")
	require.Empty(t, rt.followID)
	if initCmd := m.chatPage.Init(); initCmd != nil {
		_ = initCmd()
	}
	m.chatPage.SetSize(100, 24)
	view := m.chatPage.View()
	require.Contains(t, view, "stored historical prompt")
}

func TestHandleAttachSessionUsesStoreItemsHistoryAfterCompletedChild(t *testing.T) {
	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	root := session.New(session.WithID("root"))
	require.NoError(t, store.AddSession(ctx, root))
	storedChild := session.NewRuntimeManagedSubSession(root, session.WithID("child"), session.WithTitle("Stored Child Title"))
	require.NoError(t, store.UpdateSession(ctx, storedChild))
	_, err := store.AddMessage(ctx, "child", session.UserMessage("stored item prompt"))
	require.NoError(t, err)
	_, err = store.AddMessage(ctx, "child", &session.Message{AgentName: "greppy", Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "stored item reply"}})
	require.NoError(t, err)

	rt := &attachHydrationRuntime{store: store, rootTitle: "Root Title"}
	rootApp := app.New(ctx, rt, root)
	m := New(ctx, func(context.Context, string) (*app.App, *session.Session, func(), error) { return nil, nil, nil, nil }, rootApp, "", nil).(*appModel)

	model, cmd := m.handleAttachSession("child")
	require.NotNil(t, cmd)
	m = model.(*appModel)
	require.Equal(t, "child", m.application.Session().ID)
	require.True(t, m.application.IsReadOnly())
	require.Len(t, m.application.Session().GetAllMessages(), 2)
	require.Contains(t, m.application.Session().GetAllMessages()[0].Message.Content, "stored item prompt")
	require.Contains(t, m.application.Session().GetAllMessages()[1].Message.Content, "stored item reply")
	require.Equal(t, 0, rt.attachCalls)

	if initCmd := m.chatPage.Init(); initCmd != nil {
		_ = initCmd()
	}
	m.chatPage.SetSize(100, 24)
	view := m.chatPage.View()
	require.Contains(t, view, "stored item prompt")
	require.Contains(t, view, "stored item reply")
	require.ErrorContains(t, m.application.FollowUpWithAttachments("nope", nil), "follow-up")
}

func TestHandleAttachSessionMissingStoreAndMissingLiveErrorsGracefully(t *testing.T) {
	ctx := t.Context()
	root := session.New(session.WithID("root"))
	rt := &attachHydrationRuntime{rootTitle: "Root Title"}
	rootApp := app.New(ctx, rt, root)
	m := New(ctx, func(context.Context, string) (*app.App, *session.Session, func(), error) { return nil, nil, nil, nil }, rootApp, "", nil).(*appModel)

	model, cmd := m.handleAttachSession("missing")
	require.NotNil(t, cmd)
	require.Same(t, m, model)
	require.Equal(t, "root", m.application.Session().ID)
}

func TestHandleAttachSessionUsesLiveTitleAndTitleEventsUpdateTab(t *testing.T) {
	ctx := t.Context()
	root := session.New(session.WithID("root"))
	liveChild := session.NewRuntimeManagedSubSession(root, session.WithID("child"))
	rt := &attachHydrationRuntime{live: liveChild, childTitle: "Live Child Title"}
	rootApp := app.New(ctx, rt, root)
	m := New(ctx, func(context.Context, string) (*app.App, *session.Session, func(), error) { return nil, nil, nil, nil }, rootApp, "", nil).(*appModel)

	model, cmd := m.handleAttachSession("child")
	require.NotNil(t, cmd)
	m = model.(*appModel)
	require.Equal(t, "Live Child Title", m.application.Session().Title)
	require.Equal(t, "Live Child Title", m.sessionState.SessionTitle())

	model, _ = m.Update(runtime.SessionTitle("child", "Updated Child Title"))
	m = model.(*appModel)
	require.Equal(t, "Updated Child Title", m.sessionState.SessionTitle())
	require.Equal(t, "Updated Child Title", m.supervisor.GetRunner("child").Title)
	require.NoError(t, m.application.FollowUpWithAttachments("hello child", nil))
	require.Equal(t, "child", rt.followID)
}

func TestHandleAttachSessionOpensPersistedChildAfterReloadWithoutLiveTree(t *testing.T) {
	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	root := session.New(session.WithID("root"), session.WithTitle("Root Title"))
	require.NoError(t, store.AddSession(ctx, root))
	storedChild := session.NewRuntimeManagedSubSession(root, session.WithID("child"), session.WithTitle("Persisted Child"), session.WithAgentName("greppy"))
	storedChild.AddMessage(session.UserMessage("stored prompt after reload"))
	storedChild.AddMessage(&session.Message{AgentName: "greppy", Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "stored reply after reload"}})
	require.NoError(t, store.AddSubSession(ctx, "root", storedChild))

	rt := &attachHydrationRuntime{store: store, rootTitle: "Root Title"}
	rootApp := app.New(ctx, rt, root)
	m := New(ctx, func(context.Context, string) (*app.App, *session.Session, func(), error) { return nil, nil, nil, nil }, rootApp, "", nil).(*appModel)

	model, cmd := m.handleAttachSession("child")
	require.NotNil(t, cmd)
	m = model.(*appModel)
	require.Equal(t, "child", m.application.Session().ID)
	require.Equal(t, "Persisted Child", m.application.Session().Title)
	require.True(t, m.application.IsReadOnly())
	require.Len(t, m.application.Session().GetAllMessages(), 2)
	require.Contains(t, m.application.Session().GetAllMessages()[0].Message.Content, "stored prompt after reload")
	require.Equal(t, 0, rt.attachCalls)
	require.ErrorContains(t, m.application.FollowUpWithAttachments("nope", nil), "follow-up")

	if initCmd := m.chatPage.Init(); initCmd != nil {
		_ = initCmd()
	}
	m.chatPage.SetSize(100, 24)
	view := m.chatPage.View()
	require.Contains(t, view, "stored prompt after reload")
	require.Contains(t, view, "stored reply after reload")
}

func TestHandleAttachSessionFallsBackToParentEmbeddedHistoryWhenDirectChildIsEmpty(t *testing.T) {
	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	root := session.New(session.WithID("root"))
	embeddedChild := session.NewRuntimeManagedSubSession(root, session.WithID("child"), session.WithTitle("Embedded Child"))
	embeddedChild.AddMessage(session.UserMessage("parent embedded prompt"))
	embeddedChild.AddMessage(&session.Message{AgentName: "greppy", Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "parent embedded reply"}})
	root.AddSubSession(embeddedChild)
	require.NoError(t, store.AddSession(ctx, root))
	emptyDirectChild := session.NewRuntimeManagedSubSession(root, session.WithID("child"), session.WithTitle("Direct Child"))
	require.NoError(t, store.UpdateSession(ctx, emptyDirectChild))

	rt := &attachHydrationRuntime{store: store, rootTitle: "Root Title"}
	rootApp := app.New(ctx, rt, root)
	m := New(ctx, func(context.Context, string) (*app.App, *session.Session, func(), error) { return nil, nil, nil, nil }, rootApp, "", nil).(*appModel)

	model, cmd := m.handleAttachSession("child")
	require.NotNil(t, cmd)
	m = model.(*appModel)
	require.Equal(t, "child", m.application.Session().ID)
	require.Len(t, m.application.Session().Messages, 2)
	require.Contains(t, m.application.Session().Messages[0].Message.Message.Content, "parent embedded prompt")
	require.Contains(t, m.application.Session().Messages[1].Message.Message.Content, "parent embedded reply")
	require.True(t, m.application.IsReadOnly())
	require.Equal(t, 0, rt.attachCalls)

	if initCmd := m.chatPage.Init(); initCmd != nil {
		_ = initCmd()
	}
	m.chatPage.SetSize(100, 24)
	view := m.chatPage.View()
	require.Contains(t, view, "parent embedded prompt")
	require.Contains(t, view, "parent embedded reply")
	require.ErrorContains(t, m.application.FollowUpWithAttachments("nope", nil), "follow-up")
}
