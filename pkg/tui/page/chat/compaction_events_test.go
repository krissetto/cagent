package chat

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
)

// subCompactionEvent builds a completed compaction event for the given session.
func subCompactionEvent(sessionID, outcome string) *runtime.SessionCompactionEvent {
	return &runtime.SessionCompactionEvent{
		Type:         "session_compaction",
		SessionID:    sessionID,
		Status:       "completed",
		Outcome:      outcome,
		AgentContext: runtime.AgentContext{AgentName: "worker"},
	}
}

// TestSubSessionCompactionKeepsRootWorkState pins the #3439 event-routing
// contract: a sub-agent session finishing a compaction must not flip the
// root chat idle, clear its stream cancel func, or process root queues.
func TestSubSessionCompactionKeepsRootWorkState(t *testing.T) {
	t.Parallel()

	sess := session.New()
	p := New(animation.NewRuntime(), t.Context(), app.New(t.Context(), queueTestRuntime{}, sess), service.NewSessionState(sess)).(*chatPage)

	_, cancel := context.WithCancel(t.Context())
	defer cancel()
	p.msgCancel = cancel
	p.working = true
	p.messageQueue = []queuedMessage{{content: "queued while working"}}

	handled, _ := p.handleRuntimeEvent(subCompactionEvent("sub-session-1", runtime.CompactionOutcomeApplied))
	require.True(t, handled)

	assert.True(t, p.working, "a sub-session compaction must not mark the root chat idle")
	assert.NotNil(t, p.msgCancel, "the root stream cancel func must stay intact")
	assert.Len(t, p.messageQueue, 1, "root queued messages must not be processed")
}

// TestRootSessionCompactionResetsWorkState pins the complementary root path:
// the root session's own compaction completion still cleans up.
func TestRootSessionCompactionResetsWorkState(t *testing.T) {
	t.Parallel()

	sess := session.New()
	p := New(animation.NewRuntime(), t.Context(), app.New(t.Context(), queueTestRuntime{}, sess), service.NewSessionState(sess)).(*chatPage)

	_, cancel := context.WithCancel(t.Context())
	defer cancel()
	p.msgCancel = cancel
	p.working = true

	handled, _ := p.handleRuntimeEvent(subCompactionEvent(sess.ID, runtime.CompactionOutcomeApplied))
	require.True(t, handled)

	assert.False(t, p.working, "the root compaction completion marks the chat idle")
	assert.Nil(t, p.msgCancel)
}

// TestSubSessionCompactionNotice verifies the agent-scoped feedback for each
// terminal outcome and the silence of non-terminal events.
func TestSubSessionCompactionNotice(t *testing.T) {
	t.Parallel()

	assert.Nil(t, subSessionCompactionNotice(&runtime.SessionCompactionEvent{Status: "started"}),
		"the request was already announced; started stays silent")

	for _, outcome := range []string{"", runtime.CompactionOutcomeApplied, runtime.CompactionOutcomeSkipped, runtime.CompactionOutcomeFailed} {
		assert.NotNil(t, subSessionCompactionNotice(subCompactionEvent("sub-1", outcome)),
			"outcome %q must produce user feedback", outcome)
	}
}

// TestIsSubSessionEvent covers the root/sub classification, including the
// empty-session-ID backward-compatibility case.
func TestIsSubSessionEvent(t *testing.T) {
	t.Parallel()

	sess := session.New()
	p := New(animation.NewRuntime(), t.Context(), app.New(t.Context(), queueTestRuntime{}, sess), service.NewSessionState(sess)).(*chatPage)

	assert.False(t, p.isSubSessionEvent(""), "events without a session ID belong to the root")
	assert.False(t, p.isSubSessionEvent(sess.ID))
	assert.True(t, p.isSubSessionEvent("some-other-session"))
}
