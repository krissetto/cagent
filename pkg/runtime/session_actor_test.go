package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

// multiRunProvider hands out a fresh scripted stream per model call, so a
// session can run multiple turns (initial run + actor wakes).
type multiRunProvider struct {
	*mockProvider

	build func() *mockStream
}

func (p *multiRunProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	return p.build(), nil
}

func newActorFixture(t *testing.T) (*LocalRuntime, *session.Session) {
	t.Helper()
	prov := &multiRunProvider{
		mockProvider: &mockProvider{id: "test/mock-model"},
		build: func() *mockStream {
			return newStreamBuilder().AddContent("ok").AddStopWithUsage(1, 1).Build()
		},
	}
	tm := team.New(team.WithAgents(agent.New("root", "prompt", agent.WithModel(prov))))
	rt, err := NewLocalRuntime(t.Context(), tm)
	require.NoError(t, err)
	t.Cleanup(rt.subagents.Close)
	return rt, session.New(session.WithID("actor-sess"))
}

// A note delivered to an idle session with no receiver wakes it: the actor
// starts a runtime-owned run whose opening steer drain turns the note into a
// regular user message, and every hub subscriber sees the turn. This is what
// lets async subagents report to parents on RunStream-only hosts (API
// server, adapters) with no embedder wiring at all.
func TestActorWakesIdleSessionOnNote(t *testing.T) {
	t.Parallel()

	rt, sess := newActorFixture(t)

	// First turn, embedder-driven: registers the session with the actor.
	sess.AddMessage(session.UserMessage("hi"))
	for range rt.RunStream(t.Context(), sess) {
	}
	turnOne := len(sess.Messages)

	_, events, cancel := rt.SubscribeSessionEvents(sess.ID)
	defer cancel()

	rt.deliverOrBuffer(t.Context(), sess.ID, "<system_info>subagent report</system_info>")

	deadline := time.After(5 * time.Second)
	for {
		select {
		case e := <-events:
			if _, ok := e.(*StreamStoppedEvent); ok {
				assert.Greater(t, len(sess.Messages), turnOne,
					"the wake run must add the note and a fresh assistant reply")
				last := sess.Messages[len(sess.Messages)-1]
				require.NotNil(t, last.Message)
				assert.Equal(t, chat.MessageRoleAssistant, last.Message.Message.Role)
				// Cleanup (endSessionRun, waker exit) happens after the stop
				// event is published; settle is therefore eventual.
				assert.Eventually(t, func() bool { return rt.sessionSettled(sess.ID) },
					2*time.Second, 10*time.Millisecond)
				return
			}
		case <-deadline:
			t.Fatal("no wake run happened for the buffered note")
		}
	}
}

// A note for a session the actor has never seen stays buffered (nothing to
// run), and receiver-managed sessions are never run unattended — the
// interactive embedder owns tool approvals and elicitations.
func TestActorNeverRunsUnknownOrReceiverManagedSessions(t *testing.T) {
	t.Parallel()

	rt, sess := newActorFixture(t)

	// Unknown session: buffered, no run.
	rt.deliverOrBuffer(t.Context(), "never-ran", "note")
	assert.False(t, rt.sessionSettled("never-ran"), "note must stay buffered")
	rt.sessionRunsMu.Lock()
	assert.False(t, rt.sessionWaking["never-ran"])
	rt.sessionRunsMu.Unlock()

	// Receiver-managed session (sticky, even after unregistering): buffered.
	sess.AddMessage(session.UserMessage("hi"))
	for range rt.RunStream(t.Context(), sess) {
	}
	unregister := rt.RegisterMessageReceiver(sess.ID, func(context.Context, string) {})
	unregister()
	rt.deliverOrBuffer(t.Context(), sess.ID, "note")
	rt.sessionRunsMu.Lock()
	defer rt.sessionRunsMu.Unlock()
	assert.False(t, rt.sessionWaking[sess.ID], "interactive sessions are never run unattended")
	assert.Len(t, rt.sessionSteer[sess.ID], 1)
}

// RunOrAttach on a free session is byte-for-byte the classic embedder turn:
// messages staged on the session, the run's own stream returned, closing at
// the end of the turn.
func TestRunOrAttachDrivesFreeSession(t *testing.T) {
	t.Parallel()

	rt, sess := newActorFixture(t)

	sess.AddMessage(session.UserMessage("hello"))
	var sawStop bool
	for e := range rt.RunOrAttach(t.Context(), sess) {
		if _, ok := e.(*StreamStoppedEvent); ok {
			sawStop = true
		}
	}
	assert.True(t, sawStop)
	require.GreaterOrEqual(t, len(sess.Messages), 2)
	require.NotNil(t, sess.Messages[0].Message)
	assert.Equal(t, "hello", sess.Messages[0].Message.Message.Content)
	assert.Equal(t, chat.MessageRoleUser, sess.Messages[0].Message.Message.Role)
	assert.Eventually(t, func() bool { return rt.sessionSettled(sess.ID) },
		2*time.Second, 10*time.Millisecond)
}

// RunOrAttach while a runtime-owned run is live does not start a second
// driver: it mirrors the live run through the hub and, once the session
// settles, becomes the driver and runs the staged turn to completion.
func TestRunOrAttachMirrorsLiveRunThenDrives(t *testing.T) {
	t.Parallel()

	rt, sess := newActorFixture(t)
	rt.rememberSession(sess)
	rt.sessionRunsMu.Lock()
	rt.sessionWaking[sess.ID] = true // a wake loop owns the session
	rt.sessionRunsMu.Unlock()

	sess.AddMessage(session.UserMessage("queued"))
	ch := rt.RunOrAttach(t.Context(), sess)

	// Wait for the mirror's hub subscription before publishing, else the
	// simulated events race past it.
	require.Eventually(t, func() bool {
		rt.sessionEvents.mu.Lock()
		defer rt.sessionEvents.mu.Unlock()
		return len(rt.sessionEvents.subs[sess.ID]) == 1
	}, 2*time.Second, 5*time.Millisecond)

	// Simulate the tail of the live wake run…
	rt.sessionEvents.Publish(sess.ID, AgentChoice("root", sess.ID, "report handled"))
	rt.sessionRunsMu.Lock()
	delete(rt.sessionWaking, sess.ID)
	rt.sessionRunsMu.Unlock()
	rt.sessionEvents.Publish(sess.ID, StreamStopped(sess.ID, "root", ""))

	// …then the caller's own turn runs for real and the channel closes.
	var mirrored bool
	var stops int
	for e := range ch {
		switch ev := e.(type) {
		case *AgentChoiceEvent:
			if ev.Content == "report handled" {
				mirrored = true
			}
		case *StreamStoppedEvent:
			stops++
		}
	}
	assert.True(t, mirrored, "the live run's tail is mirrored to the caller")
	assert.Equal(t, 2, stops, "mirrored stop + the staged turn's own stop")
	last := sess.Messages[len(sess.Messages)-1]
	require.NotNil(t, last.Message)
	assert.Equal(t, chat.MessageRoleAssistant, last.Message.Message.Role,
		"the staged turn must be answered by the follow-up drive")
}
