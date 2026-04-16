package sidebar

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func TestSidebar_DelegationClickMap_UsesActualRenderedOffsets(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sessionState := service.NewSessionState(sess)
	sb := New(sessionState)
	m := sb.(*model)
	m.mode = ModeVertical
	m.width = 40
	m.height = 30
	m.AddDelegation("ab123", "coder", "first task", "child-1")
	m.AddDelegation("cd456", "reviewer", "second task", "child-2")

	_ = sb.View()

	require.NotEmpty(t, m.cachedLines)
	require.NotEmpty(t, m.delegationClickMap)

	assert.Equal(t, 0, m.delegationClickMap[2])
	assert.Equal(t, 0, m.delegationClickMap[3])
	assert.Equal(t, 0, m.delegationClickMap[4])
	assert.Equal(t, 0, m.delegationClickMap[5])

	// Ensure no phantom first blank line is mapped.
	_, found := m.delegationClickMap[1]
	assert.False(t, found)
}

func TestSidebar_HandleClick_DelegationAccountsForSidebarPosition(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sessionState := service.NewSessionState(sess)
	sb := New(sessionState)
	m := sb.(*model)
	m.mode = ModeVertical
	m.width = 40
	m.height = 30
	m.SetPosition(10, 5)
	m.AddDelegation("ab123", "coder", "first task", "child-1")

	_ = sb.View()
	require.NotEmpty(t, m.delegationClickMap)

	var clickedLine int
	for line, idx := range m.delegationClickMap {
		if idx == 0 {
			clickedLine = line
			break
		}
	}

	msg := tea.MouseClickMsg{X: 12, Y: 5 + clickedLine, Button: tea.MouseLeft}
	_, cmd := sb.Update(msg)
	require.NotNil(t, cmd)
	result := cmd()
	openMsg, ok := result.(messages.OpenChildSessionMsg)
	require.True(t, ok)
	assert.Equal(t, "child-1", openMsg.ChildSessionID)
	assert.Equal(t, "ab123", openMsg.DelegationID)
}
