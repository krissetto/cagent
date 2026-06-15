package sidebar

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func TestAttachedSidebarRendersClickableHoverableParentRow(t *testing.T) {
	store := session.NewInMemorySessionStore()
	parent := session.New(session.WithID("parent"), session.WithWorkingDir(t.TempDir()), session.WithAgentName("director"))
	child := session.NewRuntimeManagedSubSession(parent, session.WithID("child"), session.WithWorkingDir(parent.WorkingDir), session.WithAgentName("implementer"))
	require.NoError(t, store.AddSession(t.Context(), parent))
	require.NoError(t, store.AddSession(t.Context(), child))

	sb := New(service.NewSessionState(child))
	m := sb.(*model)
	m.mode = ModeVertical
	m.width = 60
	m.height = 20
	sb.LoadFromSession(child)
	sb.SetPersistedSessionTree(child, store)

	plain := ansi.Strip(sb.View())
	require.Contains(t, plain, "parent: director")

	result, id := sb.HandleClickType(m.layoutCfg.PaddingLeft+2, m.parentLineY())
	require.Equal(t, ClickParent, result)
	require.Equal(t, "parent", id)

	before := sb.View()
	model, cmd := sb.Update(tea.MouseMotionMsg{X: m.layoutCfg.PaddingLeft + 2, Y: m.parentLineY()})
	if cmd != nil {
		_ = cmd()
	}
	sb = model.(Model)
	after := sb.View()
	assert.NotEqual(t, before, after, "hovering parent row should restyle it")
	assert.Contains(t, ansi.Strip(after), "parent: director")
}
