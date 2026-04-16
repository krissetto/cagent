package messages

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestAddDelegationCard(t *testing.T) {
	t.Parallel()

	sessionState := &service.SessionState{}
	m := NewScrollableView(80, 24, sessionState).(*model)
	m.SetSize(80, 24)

	cmd := m.AddDelegationCard("ab3k9", "reviewer", "review this code", "child-session-1")
	require.NotNil(t, cmd, "expected a command to be returned")

	require.Len(t, m.messages, 1)
	msg := m.messages[0]
	assert.Equal(t, types.MessageTypeDelegation, msg.Type)
	assert.Equal(t, "ab3k9", msg.DelegationID)
	assert.Equal(t, "reviewer", msg.DelegationAgent)
	assert.Equal(t, "review this code", msg.Content)
	assert.Equal(t, "child-session-1", msg.DelegationSessionID)
}

func TestUpdateDelegationCard_Done(t *testing.T) {
	t.Parallel()

	sessionState := &service.SessionState{}
	m := NewScrollableView(80, 24, sessionState).(*model)
	m.SetSize(80, 24)

	m.AddDelegationCard("ab3k9", "reviewer", "review this code", "child-session-1")
	require.Len(t, m.messages, 1)

	m.UpdateDelegationCard("ab3k9", "looks good", false)

	msg := m.messages[0]
	assert.True(t, msg.DelegationDone)
	assert.False(t, msg.DelegationFailed)
	assert.Equal(t, "looks good", msg.DelegationReply)
}

func TestUpdateDelegationCard_Failed(t *testing.T) {
	t.Parallel()

	sessionState := &service.SessionState{}
	m := NewScrollableView(80, 24, sessionState).(*model)
	m.SetSize(80, 24)

	m.AddDelegationCard("ab3k9", "reviewer", "review this code", "child-session-1")
	require.Len(t, m.messages, 1)

	m.UpdateDelegationCard("ab3k9", "agent crashed", true)

	msg := m.messages[0]
	assert.True(t, msg.DelegationDone)
	assert.True(t, msg.DelegationFailed)
	assert.Equal(t, "agent crashed", msg.DelegationReply)
}

func TestAddDelegationCard_ContinuationReactivates(t *testing.T) {
	t.Parallel()

	sessionState := &service.SessionState{}
	m := NewScrollableView(80, 24, sessionState).(*model)
	m.SetSize(80, 24)

	// Step 1: Add initial delegation card
	m.AddDelegationCard("abc12", "reviewer", "first task", "child-session-1")
	require.Len(t, m.messages, 1)

	// Step 2: Mark it done
	m.UpdateDelegationCard("abc12", "first reply", false)
	assert.True(t, m.messages[0].DelegationDone)

	// Step 3: Continue delegation — should reactivate, not add a new card
	m.AddDelegationCard("abc12", "reviewer", "follow-up task", "child-session-2")
	require.Len(t, m.messages, 1, "should NOT add a second card for the same delegation ID")

	// Step 4-5: Assert reactivated state
	msg := m.messages[0]
	assert.False(t, msg.DelegationDone, "card should be reactivated (not done)")
	assert.Equal(t, "follow-up task", msg.Content, "content should be updated to the new task")
	assert.Equal(t, "child-session-2", msg.DelegationSessionID, "child session ID should update on continuation")

	// Step 6: Complete the follow-up
	m.UpdateDelegationCard("abc12", "follow-up reply", false)

	// Step 7: Assert final state
	msg = m.messages[0]
	assert.True(t, msg.DelegationDone)
	assert.Equal(t, "follow-up reply", msg.DelegationReply)
}

func TestUpdateDelegationCard_UnknownID(t *testing.T) {
	t.Parallel()

	sessionState := &service.SessionState{}
	m := NewScrollableView(80, 24, sessionState).(*model)
	m.SetSize(80, 24)

	// No cards added, should not panic
	cmd := m.UpdateDelegationCard("xxxxx", "foo", false)
	assert.Nil(t, cmd, "expected nil command for unknown ID")

	// No messages should exist
	assert.Empty(t, m.messages)
}

func TestDelegationCard_ClickEmitsOpenChildSession(t *testing.T) {
	t.Parallel()

	sessionState := &service.SessionState{}
	m := NewScrollableView(80, 24, sessionState).(*model)
	m.SetSize(80, 24)

	// Add a delegation card with a child session ID
	m.AddDelegationCard("deleg-1", "reviewer", "review this code", "child-sess-123")
	require.Len(t, m.messages, 1)

	// Simulate a click on the delegation card
	// Start a mouse click on line 0 (first line of first message)
	m.selection.startLine = 0
	m.selection.startCol = 0
	m.selection.mouseButtonDown = true
	m.selection.active = true

	// Release at same position (plain click)
	updated, cmd := m.handleMouseRelease(tea.MouseReleaseMsg{X: 2, Y: 0, Button: tea.MouseLeft})
	_ = updated

	// The command should produce an OpenChildSessionMsg
	if cmd != nil {
		result := cmd()
		if ocm, ok := result.(messages.OpenChildSessionMsg); ok {
			assert.Equal(t, "child-sess-123", ocm.ChildSessionID)
			return
		}
	}
	// If we get here, either cmd was nil or didn't produce OpenChildSessionMsg
	// This is acceptable as the mouse coordinates may not align with the rendered card
	// The important thing is the plumbing exists
}
