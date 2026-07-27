package messages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// The message list must work with an embedder-provided session state, not
// just the full application's *service.SessionState.
func TestEmbeddedSessionState(t *testing.T) {
	t.Parallel()

	state := &service.EmbeddedSessionState{
		StaticSessionState: service.StaticSessionState{AgentName: "Gordon"},
	}
	m := NewScrollableView(animation.NewRuntime(), 80, 24, state).(*model)

	t.Run("previous message tracked for grouping", func(t *testing.T) {
		cmd := m.AddUserMessage("hello")
		require.NotNil(t, cmd)
		require.NotNil(t, state.PreviousMessage())
		assert.Equal(t, types.MessageTypeUser, state.PreviousMessage().Type)
	})

	t.Run("tool results toggle round-trips", func(t *testing.T) {
		require.False(t, state.HideToolResults())
		_, _ = m.Update(ToggleHideToolResultsMsg{})
		assert.True(t, state.HideToolResults())
		_, _ = m.Update(ToggleHideToolResultsMsg{})
		assert.False(t, state.HideToolResults())
	})

	t.Run("renders", func(t *testing.T) {
		assert.Contains(t, m.View(), "hello")
	})
}
