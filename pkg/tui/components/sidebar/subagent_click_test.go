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
