package sidebar

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func newTurnLifecycleSidebarModel(t *testing.T) *model {
	t.Helper()
	sess := session.New()
	ss := service.NewSessionState(sess)
	ss.SetAvailableAgents([]runtime.AgentDetails{{Name: "root", Provider: "openai", Model: "gpt-4.1"}})
	ss.SetCurrentAgentName("root")

	m := New(ss).(*model)
	m.SetTeamInfo([]runtime.AgentDetails{{Name: "root", Provider: "openai", Model: "gpt-4.1"}})
	m.width = 60
	m.height = 20
	m.currentAgent = "root"
	m.sessionHasContent = true
	m.titleGenerated = true
	m.sessionTitle = "Test"
	return m
}

func TestSidebar_StreamAndTurnLifecycleSplit(t *testing.T) {
	t.Parallel()
	m := newTurnLifecycleSidebarModel(t)

	_, _ = m.Update(runtime.StreamStarted("session-1", "root"))
	assert.Equal(t, "session-1", m.currentSessionID)
	assert.Empty(t, m.workingAgent, "StreamStartedEvent must no longer set per-turn working agent")

	streamOnly := ansi.Strip(m.agentInfo(50))
	assert.Contains(t, streamOnly, "▶ root",
		"without TurnStartedEvent the current-agent row should stay in its non-working state")
	assert.NotContains(t, streamOnly, ansi.Strip(m.spinner.RawFrame()))

	_, _ = m.Update(runtime.TurnStarted("session-1", "root"))
	assert.Equal(t, "root", m.workingAgent, "TurnStartedEvent should activate the current agent row")

	working := ansi.Strip(m.agentInfo(50))
	assert.Contains(t, working, ansi.Strip(m.spinner.RawFrame()))
	assert.NotContains(t, working, "▶ root")

	_, _ = m.Update(runtime.TurnEnded("session-1", "root"))
	assert.Empty(t, m.workingAgent, "TurnEndedEvent should clear per-turn working state")

	ended := ansi.Strip(m.agentInfo(50))
	assert.Contains(t, ended, "▶ root")
	assert.NotContains(t, ended, ansi.Strip(m.spinner.RawFrame()))
}
