package sidebar

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func TestSidebar_HandleClickType_Subagent(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sessionState := service.NewSessionState(sess)
	sb := New(sessionState)
	m := sb.(*model)
	m.sessionHasContent = true
	m.titleGenerated = true
	m.sessionTitle = "Test"
	m.width = 50
	m.height = 50

	m.recordSubAgentUpdate(&runtime.SubAgentUpdateEvent{
		Envelope: subagent.Envelope{
			SubAgentID: "abcdef1234567890",
			AgentName:  "researcher",
			Kind:       subagent.UpdateKindTurnCompleted,
			Status:     subagent.StatusWaiting,
			Preview:    "Found a thing",
			At:         time.Now(),
		},
	})

	// Force render to populate click zones.
	_ = sb.View()

	fullID := "abcdef1234567890"
	found := false
	for y := range len(m.cachedLines) {
		result, got := sb.HandleClickType(m.layoutCfg.PaddingLeft+2, y)
		if result == ClickSubagent {
			assert.Equal(t, fullID, got)
			found = true
		}
	}
	require.True(t, found, "should be able to click a subagent row")
}

func TestSidebar_HoverSubagent(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sessionState := service.NewSessionState(sess)
	sb := New(sessionState)
	m := sb.(*model)
	m.sessionHasContent = true
	m.titleGenerated = true
	m.sessionTitle = "Test"
	m.width = 50
	m.height = 50
	// Place the sidebar at a known screen position so absolute coords work.
	m.xPos = 0
	m.yPos = 0

	const fullID = "abcdef1234567890"
	m.recordSubAgentUpdate(&runtime.SubAgentUpdateEvent{
		Envelope: subagent.Envelope{
			SubAgentID: fullID,
			AgentName:  "researcher",
			Kind:       subagent.UpdateKindTurnCompleted,
			Status:     subagent.StatusWaiting,
			Preview:    "Found a thing",
			At:         time.Now(),
		},
	})

	// Force render to populate both cachedLines and subagentClickZones.
	_ = sb.View()
	require.NotEmpty(t, m.subagentClickZones, "subagentClickZones should be populated after View()")

	// Find the screen line that maps to our subagent.
	var subagentScreenY int
	found := false
	for lineY, id := range m.subagentClickZones {
		if id == fullID {
			subagentScreenY = lineY - m.scrollview.ScrollOffset()
			found = true
			break
		}
	}
	require.True(t, found, "subagent should have a click zone")

	// Simulate mouse motion over the subagent row.
	changed := m.updateSubagentHoverAt(m.layoutCfg.PaddingLeft+2, subagentScreenY)
	assert.True(t, changed, "first hover should report changed=true")
	assert.Equal(t, fullID, m.hoveredSubagentID, "hoveredSubagentID should be set to the subagent")

	// Moving over the same row again should not report changed.
	changed = m.updateSubagentHoverAt(m.layoutCfg.PaddingLeft+2, subagentScreenY)
	assert.False(t, changed, "second hover on same row should be unchanged")

	// Moving off the row (e.g. y=0, the title area) should clear the hover.
	changed = m.updateSubagentHoverAt(m.layoutCfg.PaddingLeft+2, 0)
	assert.True(t, changed, "moving off the row should report changed=true")
	assert.Empty(t, m.hoveredSubagentID, "hoveredSubagentID should be cleared")
}
