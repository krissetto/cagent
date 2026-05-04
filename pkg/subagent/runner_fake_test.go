package subagent

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
)

// TestFakeRunner_MinimalRunnerContract demonstrates that the Manager
// works correctly with ANY Runner implementation that drives Handle
// exclusively through the Publish* methods and reacts to CloseCh /
// InboxSignal. This serves as both a regression check (the contract
// stays minimal) and a template for alternate in-process or remote
// runner implementations.
func TestFakeRunner_MinimalRunnerContract(t *testing.T) {
	t.Parallel()

	parent := session.New(session.WithUserMessage("root"))

	// A minimal runner that completes one "turn" then publishes a turn
	// envelope, forwards parent messages as new turns, and exits on close.
	fr := fakeRunner{start: func(ctx context.Context, h *Handle) <-chan struct{} {
		done := make(chan struct{})
		go func() {
			defer close(done)
			h.MarkRunning()

			// Simulate one turn of work.
			h.PublishTurn("first reply")

			// Wait for new input or close.
			for {
				select {
				case <-ctx.Done():
					h.PublishStopped()
					return
				case <-h.CloseCh():
					h.PublishClosed()
					return
				case <-h.InboxSignal():
					msgs := h.DrainInbox()
					if len(msgs) == 0 {
						continue
					}
					h.MarkRunning()
					// Simulate processing the message.
					h.PublishTurn("reply to: " + msgs[0].Content)
				}
			}
		}()
		return done
	}}

	mgr := NewManager(fr)
	child := session.New(session.WithAgentName("fake-worker"))

	h, err := mgr.StartChild(t.Context(), StartConfig{
		Parent:    parent,
		AgentName: "fake-worker",
		Task:      "test the contract",
	}, child)
	require.NoError(t, err)

	// First turn envelope arrives at parent inbox.
	require.True(t, mgr.WaitParentInbox(t.Context(), parent.ID))
	envs := mgr.DrainParentInbox(parent.ID)
	require.Len(t, envs, 1)
	assert.Equal(t, UpdateKindTurnCompleted, envs[0].Kind)
	assert.Equal(t, "first reply", envs[0].Preview)

	// Send a follow-up message.
	require.NoError(t, mgr.Send(h.ID(), Message{Content: "continue"}))
	require.True(t, mgr.WaitParentInbox(t.Context(), parent.ID))
	envs = mgr.DrainParentInbox(parent.ID)
	require.Len(t, envs, 1)
	assert.Contains(t, envs[0].Preview, "reply to: continue")

	// Close the subagent.
	require.NoError(t, mgr.Close(h.ID()))
	require.True(t, mgr.WaitParentInbox(t.Context(), parent.ID))
	envs = mgr.DrainParentInbox(parent.ID)
	require.Len(t, envs, 1)
	assert.Equal(t, UpdateKindClosed, envs[0].Kind)

	// Loop done should be closed.
	select {
	case <-h.LoopDone():
	case <-time.After(time.Second):
		t.Fatal("expected loop to exit after close")
	}
}
