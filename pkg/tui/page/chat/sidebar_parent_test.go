package chat

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/session"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

type parentLinkRuntime struct {
	queueTestRuntime

	store session.Store
}

func (r parentLinkRuntime) SessionStore() session.Store { return r.store }

func TestAttachedSidebarParentRowLinksToParentSession(t *testing.T) {
	store := session.NewInMemorySessionStore()
	parent := session.New(session.WithID("parent-session"), session.WithWorkingDir(t.TempDir()), session.WithAgentName("director"))
	child := session.NewRuntimeManagedSubSession(parent, session.WithID("child-session"), session.WithWorkingDir(parent.WorkingDir), session.WithAgentName("implementer"))
	require.NoError(t, store.AddSession(t.Context(), parent))
	require.NoError(t, store.AddSession(t.Context(), child))

	p := New(app.New(t.Context(), parentLinkRuntime{store: store}, child), service.NewSessionState(child)).(*chatPage)
	p.SetSize(140, 24)
	cmd := p.Init()
	if cmd != nil {
		_ = cmd()
	}
	_ = p.View()

	sl := p.computeSidebarLayout()
	require.Equal(t, sidebarVertical, sl.mode)

	// Session tab layout: header, top padding, title, blank separator,
	// working directory, then parent row.
	parentY := 5
	parentX := styles.AppPadding + sl.sidebarStartX + 2
	hit := NewHitTest(p)
	require.Equal(t, TargetSidebarParent, hit.At(parentX, parentY))
	require.Equal(t, "parent-session", hit.ParentSessionID)

	_, clickCmd := p.handleMouseClick(tea.MouseClickMsg{X: parentX, Y: parentY, Button: tea.MouseLeft})
	require.NotNil(t, clickCmd)
	attach, ok := clickCmd().(msgtypes.AttachSessionMsg)
	require.True(t, ok)
	require.Equal(t, "parent-session", attach.SessionID)
}
