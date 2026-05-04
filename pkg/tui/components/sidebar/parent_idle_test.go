package sidebar

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func TestSidebar_ParentIdleStopsCurrentAgentRowSpinner(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sessionState := service.NewSessionState(sess)
	sessionState.SetAvailableAgents([]runtime.AgentDetails{{Name: "root", Provider: "openai", Model: "gpt-4.1"}})
	sessionState.SetCurrentAgentName("root")

	m := New(sessionState).(*model)
	m.SetTeamInfo([]runtime.AgentDetails{{Name: "root", Provider: "openai", Model: "gpt-4.1"}})
	m.width = 60
	m.height = 20
	m.currentAgent = "root"
	m.workingAgent = "root"
	m.sessionHasContent = true
	m.titleGenerated = true
	m.sessionTitle = "Test"

	active := ansi.Strip(m.agentInfo(50))
	require.Contains(t, active, ansi.Strip(m.spinner.RawFrame()), "working parent should show spinner frame in agent row")
	require.NotContains(t, active, "▶ root", "working row should not fall back to static glyph while parent is active")

	_, _ = m.Update(runtime.ParentIdle(sess.ID, "root"))
	idle := ansi.Strip(m.agentInfo(50))
	assert.NotContains(t, idle, ansi.Strip(m.spinner.RawFrame()), "parent-idle state must suppress the parent row spinner")
	assert.Contains(t, idle, "▶ root", "parent-idle state should fall back to the static current-agent glyph")

	_, _ = m.Update(runtime.ParentResume(sess.ID, "root"))
	resumed := ansi.Strip(m.agentInfo(50))
	assert.Contains(t, resumed, ansi.Strip(m.spinner.RawFrame()), "resuming the parent turn should restore the row spinner")
}
