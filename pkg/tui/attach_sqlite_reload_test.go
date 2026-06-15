package tui

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestHandleAttachSessionReloadsSQLiteChildTranscriptAfterRestart(t *testing.T) {
	ctx := t.Context()
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewSQLiteSessionStore(dbPath)
	require.NoError(t, err)
	root := session.New(session.WithID("root"), session.WithTitle("Root Title"), session.WithAgentName("root"), session.WithToolsApproved(true))
	require.NoError(t, store.AddSession(ctx, root))
	child := session.NewRuntimeManagedSubSession(root, session.WithID("child"), session.WithTitle("Generated Child Title"), session.WithAgentName("greppy"))
	require.NoError(t, store.AddSubSession(ctx, "root", child))
	_, err = store.AddMessage(ctx, "child", session.UserMessage("sqlite child prompt"))
	require.NoError(t, err)
	_, err = store.AddMessage(ctx, "child", &session.Message{AgentName: "greppy", Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "sqlite child reply"}})
	require.NoError(t, err)

	reloaded, err := session.NewSQLiteSessionStore(dbPath)
	require.NoError(t, err)
	reloadedRoot, err := reloaded.GetSession(ctx, "root")
	require.NoError(t, err)
	freshRuntime, err := runtime.NewLocalRuntime(team.New(team.WithAgents(agent.New("root", "root", agent.WithModel(sqliteReloadProvider{})))), runtime.WithSessionStore(reloaded), runtime.WithSessionCompaction(false))
	require.NoError(t, err)
	defer func() { _ = freshRuntime.Close() }()
	tree, err := freshRuntime.LiveSessionTree(ctx, "root")
	require.NoError(t, err)
	require.NotNil(t, tree.Root)
	require.Len(t, tree.Root.Children, 1)
	require.Equal(t, "child", tree.Root.Children[0].ID)
	require.False(t, tree.Root.Children[0].Live)
	require.Equal(t, "waiting", tree.Root.Children[0].Status)

	rt := &sqliteReloadAttachRuntime{store: reloaded, rootTitle: "Root Title", tree: tree}
	rootApp := app.New(ctx, rt, reloadedRoot)
	m := New(ctx, func(context.Context, string) (*app.App, *session.Session, func(), error) { return nil, nil, nil, nil }, rootApp, "", nil).(*appModel)
	if initCmd := m.chatPage.Init(); initCmd != nil {
		_ = initCmd()
	}
	m.chatPage.SetSize(140, 24)
	rootView := m.chatPage.View()
	require.Contains(t, rootView, "Subagents")
	require.Contains(t, rootView, "greppy")
	require.Contains(t, rootView, "child")
	require.Contains(t, rootView, "idle")
	require.NotContains(t, rootView, "finalized")

	model, cmd := m.handleAttachSession("child")
	require.NotNil(t, cmd)
	m = model.(*appModel)
	require.Equal(t, "child", m.application.Session().ID)
	require.Equal(t, "Generated Child Title", m.application.Session().Title)
	require.Equal(t, "Generated Child Title", m.sessionState.SessionTitle())
	require.True(t, m.application.IsReadOnly())
	require.Len(t, m.application.Session().GetAllMessages(), 2)
	require.Contains(t, m.application.Session().GetAllMessages()[0].Message.Content, "sqlite child prompt")
	require.Contains(t, m.application.Session().GetAllMessages()[1].Message.Content, "sqlite child reply")
	require.Equal(t, 0, rt.attachCalls)

	if initCmd := m.chatPage.Init(); initCmd != nil {
		_ = initCmd()
	}
	m.chatPage.SetSize(140, 24)
	view := m.chatPage.View()
	require.Contains(t, view, "sqlite child prompt")
	require.Contains(t, view, "sqlite child reply")
}

type sqliteReloadAttachRuntime struct {
	attachBaseRuntime

	store       session.Store
	rootTitle   string
	attachCalls int
	tree        *runtime.LiveSessionTree
}

func (r *sqliteReloadAttachRuntime) SessionStore() session.Store { return r.store }

func (r *sqliteReloadAttachRuntime) LiveChildSession(string) (*session.Session, bool) {
	return nil, false
}

func (r *sqliteReloadAttachRuntime) LiveSessionTree(context.Context, string) (*runtime.LiveSessionTree, error) {
	if r.tree != nil {
		return r.tree, nil
	}
	return &runtime.LiveSessionTree{Root: &runtime.LiveSessionNode{ID: "root", Title: r.rootTitle}}, nil
}

func (r *sqliteReloadAttachRuntime) AttachLiveSessionWithSnapshot(context.Context, string, int) ([]runtime.Event, <-chan runtime.Event, error) {
	r.attachCalls++
	ch := make(chan runtime.Event)
	close(ch)
	return nil, ch, nil
}

func (r *sqliteReloadAttachRuntime) AttachLiveSession(context.Context, string) (<-chan runtime.Event, func(), error) {
	r.attachCalls++
	ch := make(chan runtime.Event)
	close(ch)
	return ch, func() {}, nil
}

func (r *sqliteReloadAttachRuntime) FollowUpSessionByID(string, runtime.QueuedMessage) error {
	return nil
}
func (r *sqliteReloadAttachRuntime) SteerSessionByID(string, runtime.QueuedMessage) error { return nil }
func (r *sqliteReloadAttachRuntime) InterruptSessionByID(string) error                    { return nil }
func (r *sqliteReloadAttachRuntime) CloseSessionByID(string) error                        { return nil }
func (r *sqliteReloadAttachRuntime) StopSessionByID(string) error                         { return nil }

type sqliteReloadProvider struct{}

func (sqliteReloadProvider) ID() modelsdev.ID { return modelsdev.ParseIDOrZero("test/root") }

func (sqliteReloadProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	return nil, nil
}

func (sqliteReloadProvider) BaseConfig() base.Config { return base.Config{} }
