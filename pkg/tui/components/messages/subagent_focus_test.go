package messages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestFocusSubAgentCard_SelectsMostRecentMatchingCard(t *testing.T) {
	t.Parallel()

	sess := session.New()
	state := service.NewSessionState(sess)
	m := newModel(80, 20, state)

	// Add two cards for the same short id and one for a different id.
	m.AddSubAgentMessage(types.SubAgentInfo{Kind: types.SubAgentEventStarted, AgentName: "planner", ShortID: "abcde"})
	m.AddSubAgentMessage(types.SubAgentInfo{Kind: types.SubAgentEventStarted, AgentName: "coder", ShortID: "vwxyz"})
	m.AddSubAgentMessage(types.SubAgentInfo{Kind: types.SubAgentEventTurnCompleted, AgentName: "planner", ShortID: "abcde", Detail: "done"})

	cmd := m.FocusSubAgentCard("abcde")
	require.NotNil(t, cmd)
	assert.True(t, m.focused)
	require.GreaterOrEqual(t, m.selectedMessageIndex, 0)
	assert.Equal(t, len(m.messages)-1, m.selectedMessageIndex, "should focus the most recent matching card")
	require.NotNil(t, m.messages[m.selectedMessageIndex].SubAgent)
	assert.Equal(t, "abcde", m.messages[m.selectedMessageIndex].SubAgent.ShortID)
}

func TestFocusSubAgentCard_NoMatchNoOp(t *testing.T) {
	t.Parallel()

	sess := session.New()
	state := service.NewSessionState(sess)
	m := newModel(80, 20, state)
	m.AddSubAgentMessage(types.SubAgentInfo{Kind: types.SubAgentEventStarted, AgentName: "planner", ShortID: "abcde"})

	cmd := m.FocusSubAgentCard("xxxxx")
	assert.Nil(t, cmd)
	assert.Equal(t, -1, m.selectedMessageIndex)
}
