package tui

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

func TestHandleAttachSessionReloadsSQLiteChildTranscriptAfterRestart(t *testing.T) {
	ctx := t.Context()
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewSQLiteSessionStore(dbPath)
	require.NoError(t, err)
	root := session.New(session.WithID("root"), session.WithTitle("Root Title"))
	require.NoError(t, store.AddSession(ctx, root))
	child := session.NewRuntimeManagedSubSession(root, session.WithID("child"), session.WithTitle("Generated Child Title"), session.WithAgentName("greppy"))
	require.NoError(t, store.UpdateSession(ctx, child))
	_, err = store.AddMessage(ctx, "child", session.UserMessage("sqlite child prompt"))
	require.NoError(t, err)
	_, err = store.AddMessage(ctx, "child", &session.Message{AgentName: "greppy", Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "sqlite child reply"}})
	require.NoError(t, err)

	reloaded, err := session.NewSQLiteSessionStore(dbPath)
	require.NoError(t, err)
	reloadedRoot, err := reloaded.GetSession(ctx, "root")
	require.NoError(t, err)
	rt := &sqliteReloadAttachRuntime{store: reloaded, rootTitle: "Root Title"}
	rootApp := app.New(ctx, rt, reloadedRoot)
	m := New(ctx, func(context.Context, string) (*app.App, *session.Session, func(), error) { return nil, nil, nil, nil }, rootApp, "", nil).(*appModel)

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
	m.chatPage.SetSize(100, 24)
	view := m.chatPage.View()
	require.Contains(t, view, "sqlite child prompt")
	require.Contains(t, view, "sqlite child reply")
}

type sqliteReloadAttachRuntime struct {
	attachBaseRuntime

	store       session.Store
	rootTitle   string
	attachCalls int
}

func (r *sqliteReloadAttachRuntime) SessionStore() session.Store { return r.store }

func (r *sqliteReloadAttachRuntime) LiveChildSession(string) (*session.Session, bool) {
	return nil, false
}

func (r *sqliteReloadAttachRuntime) LiveSessionTree(context.Context, string) (*runtime.LiveSessionTree, error) {
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
