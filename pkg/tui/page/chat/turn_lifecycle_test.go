package chat

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/components/messages"
	"github.com/docker/docker-agent/pkg/tui/components/sidebar"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func newTurnLifecycleTestPage(t *testing.T) *chatPage {
	t.Helper()
	sess := session.New()
	ss := service.NewSessionState(sess)
	return &chatPage{
		sidebar:      sidebar.New(ss),
		messages:     messages.New(ss),
		sessionState: ss,
		working:      false,
		streamDepth:  0,
	}
}

func TestStreamStartedEvent_DoesNotFlipWorkingState(t *testing.T) {
	t.Parallel()
	p := newTurnLifecycleTestPage(t)

	handled, cmd := p.handleRuntimeEvent(runtime.StreamStarted("session-1", "root"))
	require.True(t, handled)
	_ = cmd // stream start may still return a sidebar/title spinner command
	assert.False(t, p.IsWorking(), "StreamStartedEvent must no longer drive per-turn working state")
	assert.Equal(t, 1, p.streamDepth)
}

func TestTurnLifecycleEvents_FlipWorkingState(t *testing.T) {
	t.Parallel()
	p := newTurnLifecycleTestPage(t)

	_, cmd := p.handleRuntimeEvent(runtime.TurnStarted("session-1", "root"))
	require.NotNil(t, cmd, "TurnStartedEvent should emit batched working/pending cmds")
	assert.True(t, p.IsWorking(), "TurnStartedEvent must enable per-turn working state")

	_, cmd = p.handleRuntimeEvent(runtime.TurnEnded("session-1", "root"))
	require.NotNil(t, cmd, "TurnEndedEvent should emit batched cleanup cmds")
	assert.False(t, p.IsWorking(), "TurnEndedEvent must clear per-turn working state")
}

func TestTurnEndedEvent_DoesNotDrainQueuedMessages(t *testing.T) {
	t.Parallel()
	p := newTurnLifecycleTestPage(t)
	p.working = true
	p.messageQueue = []queuedMessage{{content: "queued follow-up"}}

	_, cmd := p.handleRuntimeEvent(runtime.TurnEnded("session-1", "root"))
	require.NotNil(t, cmd)
	assert.Len(t, p.messageQueue, 1,
		"TurnEndedEvent must not process queued messages; StreamStoppedEvent still owns that session-lifetime transition")
	assert.False(t, p.IsWorking())
}

func TestTurnStartedEvent_NoPendingAfterAssistantContent(t *testing.T) {
	t.Parallel()
	p := newTurnLifecycleTestPage(t)

	// Simulate first turn: TurnStarted should show pending
	_, cmd := p.handleRuntimeEvent(runtime.TurnStarted("session-1", "root"))
	require.NotNil(t, cmd, "first turn should show pending spinner")
	assert.True(t, p.IsWorking())

	// Simulate assistant content arriving
	p.hasReceivedAssistantContent = true

	// TurnEnded
	p.handleRuntimeEvent(runtime.TurnEnded("session-1", "root"))

	// Second turn after tool calls: TurnStarted should NOT show pending
	// (the bottom-right working spinner is sufficient)
	_, cmd = p.handleRuntimeEvent(runtime.TurnStarted("session-1", "root"))
	require.NotNil(t, cmd, "second turn should still set working")
	assert.True(t, p.IsWorking(), "working spinner should be on")
	// The pending response spinner should NOT have been added since
	// hasReceivedAssistantContent is true.
}
