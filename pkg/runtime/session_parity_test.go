package runtime

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

// These tests serve as a safety net for the unified session engine. They lock in the invariant that
// root sessions and subagent sessions emit the same shape of
// session-scoped events for the same conceptual conversation progress,
// and that both can take multiple turns over their lifetime driven by
// their respective inbound-work primitives (root follow-up queue vs
// child inbox).
//
// The rule of thumb encoded here is:
//
//   - the in-turn event shape must not depend on whether the session
//     is a root or a subagent;
//   - both kinds of session must support being driven across multiple
//     turns;
//   - subagent-only events (SubAgentStarted, SubAgentUpdate, parent
//     idle/resume) are policy on top of the shared shape, not a
//     replacement for it.
//
// If this file ever needs to grow special-cases for "root only" or
// "child only" assertions on the in-turn events themselves, that is
// strong evidence the unification refactor has regressed.

// TestSessionParity_RootEmitsCanonicalTurnEventsThroughEventBus verifies
// that a root session publishes the canonical conversation-progress
// events (StreamStarted, AgentChoice, MessageAdded, StreamStopped) on
// the runtime event bus under the root session id. This is the
// reference shape that subagent sessions are expected to match.
func TestSessionParity_RootEmitsCanonicalTurnEventsThroughEventBus(t *testing.T) {
	setup := newSubagentTestSetup(t)
	setup.queueParentStream(t, newStreamBuilder().AddContent("hello").AddStopWithUsage(5, 2))

	sess := session.New(session.WithUserMessage("hi"), session.WithToolsApproved(true))
	ctx, cancel := timeoutCtx(t, 5*time.Second)
	defer cancel()

	sub := setup.rt.SubscribeSession(ctx, sess.ID, 256)
	t.Cleanup(sub.Cancel)

	for range setup.rt.RunStream(ctx, sess) {
	}

	got := drainBusEvents(sub)
	assertSessionScopedEventShape(t, got, sess.ID, "root", "hello")
}

// TestSessionParity_ChildEmitsCanonicalTurnEventsThroughEventBus
// verifies that a subagent session publishes the same canonical
// conversation-progress events on the event bus under the *child*
// session id. The events must be scoped to the child session, not the
// parent: this is what allows the TUI's attached-tab feature and any
// observer to render the child's transcript independently.
func TestSessionParity_ChildEmitsCanonicalTurnEventsThroughEventBus(t *testing.T) {
	setup := newSubagentTestSetup(t)

	parent := session.New(session.WithUserMessage("root"), session.WithToolsApproved(true))
	setup.queueWorkerStream(t, newStreamBuilder().AddContent("hello").AddStopWithUsage(5, 2))

	childSess := setup.rt.newSubagentChildSession(parent, subagent.StartConfig{
		Parent:        parent,
		AgentName:     "worker",
		Task:          "work",
		ToolsApproved: true,
		Title:         "child",
	}, setup.worker)

	ctx, cancel := timeoutCtx(t, 5*time.Second)
	defer cancel()

	sub := setup.rt.SubscribeSession(ctx, childSess.ID, 256)
	t.Cleanup(sub.Cancel)

	h, err := setup.rt.subagents.StartChild(ctx, subagent.StartConfig{
		Parent:        parent,
		AgentName:     "worker",
		Task:          "work",
		ToolsApproved: true,
		Title:         "child",
	}, childSess)
	require.NoError(t, err)
	t.Cleanup(func() { _ = setup.rt.subagents.Stop(h.ID()) })

	require.True(t, setup.rt.subagents.WaitParentInbox(ctx, parent.ID),
		"first child turn should publish an envelope to the parent")
	_ = setup.rt.subagents.DrainParentInbox(parent.ID)

	got := drainBusEvents(sub)
	assertSessionScopedEventShape(t, got, childSess.ID, "worker", "hello")
}

// TestSessionParity_RootMultipleTurnsViaFollowUp verifies that a root
// session can take multiple consecutive turns driven by the runtime
// follow-up queue. After each turn the canonical event shape repeats.
//
// This is the root-side analogue of TestSubagent_ParentSendAndChildSecondTurn
// and is intentionally written in a parity-friendly framing so the
// pair lives or dies together.
func TestSessionParity_RootMultipleTurnsViaFollowUp(t *testing.T) {
	setup := newSubagentTestSetup(t)
	setup.queueParentStream(t, newStreamBuilder().AddContent("turn-1").AddStopWithUsage(4, 2))
	setup.queueParentStream(t, newStreamBuilder().AddContent("turn-2").AddStopWithUsage(4, 2))

	sess := session.New(session.WithUserMessage("hi"), session.WithToolsApproved(true))
	ctx, cancel := timeoutCtx(t, 5*time.Second)
	defer cancel()

	require.NoError(t, setup.rt.FollowUp(QueuedMessage{Content: "next please"}))

	var contents strings.Builder
	for ev := range setup.rt.RunStream(ctx, sess) {
		if e, ok := ev.(*AgentChoiceEvent); ok {
			contents.WriteString(e.Content)
		}
	}
	joined := contents.String()
	assert.Contains(t, joined, "turn-1",
		"first root turn should produce the initial assistant content")
	assert.Contains(t, joined, "turn-2",
		"a queued follow-up should drive a second root turn after the first one stops")
}

// TestSessionParity_ChildMultipleTurnsViaSend verifies that a subagent
// session can take multiple consecutive turns driven by parent->child
// messages, and that each turn publishes the standard envelope to the
// parent. This is the child-side analogue of
// TestSessionParity_RootMultipleTurnsViaFollowUp.
func TestSessionParity_ChildMultipleTurnsViaSend(t *testing.T) {
	setup := newSubagentTestSetup(t)
	parent := session.New(session.WithUserMessage("root"), session.WithToolsApproved(true))

	setup.queueWorkerStream(t, newStreamBuilder().AddContent("turn-1").AddStopWithUsage(4, 2))
	setup.queueWorkerStream(t, newStreamBuilder().AddContent("turn-2").AddStopWithUsage(4, 2))

	childSess := setup.rt.newSubagentChildSession(parent, subagent.StartConfig{
		Parent:        parent,
		AgentName:     "worker",
		Task:          "work",
		ToolsApproved: true,
		Title:         "child",
	}, setup.worker)

	ctx, cancel := timeoutCtx(t, 5*time.Second)
	defer cancel()

	h, err := setup.rt.subagents.StartChild(ctx, subagent.StartConfig{
		Parent:        parent,
		AgentName:     "worker",
		Task:          "work",
		ToolsApproved: true,
		Title:         "child",
	}, childSess)
	require.NoError(t, err)
	t.Cleanup(func() { _ = setup.rt.subagents.Stop(h.ID()) })

	require.True(t, setup.rt.subagents.WaitParentInbox(ctx, parent.ID))
	envs := setup.rt.subagents.DrainParentInbox(parent.ID)
	require.Len(t, envs, 1)
	assert.Contains(t, envs[0].Preview, "turn-1",
		"first child turn should publish its assistant content to the parent inbox")

	require.NoError(t, setup.rt.subagents.Send(h.ID(), subagent.Message{Content: "continue"}))

	require.True(t, setup.rt.subagents.WaitParentInbox(ctx, parent.ID))
	envs = setup.rt.subagents.DrainParentInbox(parent.ID)
	require.Len(t, envs, 1)
	assert.Contains(t, envs[0].Preview, "turn-2",
		"a parent->child Send should drive a second child turn after the first one stops")
}

// drainBusEvents collects all events delivered to the subscription's
// channel without blocking longer than necessary. It treats a brief
// quiet period as the end of the event stream so tests do not have to
// rely on the runtime closing topics deterministically.
func drainBusEvents(sub *Subscription) []Event {
	var out []Event
	const idle = 200 * time.Millisecond
	deadline := time.NewTimer(idle)
	defer deadline.Stop()
	for {
		select {
		case ev, ok := <-sub.Events:
			if !ok {
				return out
			}
			out = append(out, ev)
			if !deadline.Stop() {
				select {
				case <-deadline.C:
				default:
				}
			}
			deadline.Reset(idle)
		case <-deadline.C:
			return out
		}
	}
}

// assertSessionScopedEventShape locks in the canonical
// conversation-progress event shape that root and child sessions must
// share. We check by event type rather than by exact event count so the
// assertion does not depend on counts of envelope/info events that can
// change as the runtime evolves.
//
// StreamStopped may or may not have been observed at drain time because
// the subscriber drains after a brief idle rather than after the stream
// fully closes. We assert StreamStarted (always arrives quickly),
// TurnStarted/TurnEnded (per-turn pair), and MessageAdded.
func assertSessionScopedEventShape(t *testing.T, events []Event, sessionID, agentName, content string) {
	t.Helper()

	var (
		streamStarted, messageAdded bool
		turnStarted, turnEnded      bool
		choiceContent               strings.Builder
	)
	for _, ev := range events {
		switch e := ev.(type) {
		case *StreamStartedEvent:
			streamStarted = true
			assert.Equal(t, sessionID, e.SessionID, "StreamStarted must be scoped to the live session")
			assert.Equal(t, agentName, e.AgentName, "StreamStarted must carry the live agent name")
		case *TurnStartedEvent:
			turnStarted = true
			assert.Equal(t, sessionID, e.SessionID, "TurnStarted must be scoped to the live session")
			assert.Equal(t, agentName, e.AgentName, "TurnStarted must carry the live agent name")
		case *TurnEndedEvent:
			turnEnded = true
			assert.Equal(t, sessionID, e.SessionID, "TurnEnded must be scoped to the live session")
			assert.Equal(t, agentName, e.AgentName, "TurnEnded must carry the live agent name")
		case *AgentChoiceEvent:
			assert.Equal(t, sessionID, e.SessionID, "AgentChoice must be scoped to the live session")
			assert.Equal(t, agentName, e.AgentName, "AgentChoice must carry the live agent name")
			choiceContent.WriteString(e.Content)
		case *MessageAddedEvent:
			messageAdded = true
			assert.Equal(t, sessionID, e.SessionID, "MessageAdded must be scoped to the live session")
			require.NotNil(t, e.Message, "MessageAdded must carry the persisted message")
			if e.Message.Message.Role == chat.MessageRoleAssistant {
				assert.Equal(t, agentName, e.AgentName, "assistant MessageAdded should reflect the producing agent")
			}
		}
	}

	assert.True(t, streamStarted, "live session %s must publish a StreamStartedEvent on its bus topic", sessionID)
	assert.True(t, turnStarted, "live session %s must publish a TurnStartedEvent on its bus topic", sessionID)
	assert.True(t, turnEnded, "live session %s must publish a TurnEndedEvent on its bus topic", sessionID)
	assert.True(t, messageAdded, "live session %s must publish a MessageAddedEvent on its bus topic", sessionID)
	assert.Contains(t, choiceContent.String(), content,
		"live session %s must publish AgentChoice deltas covering its assistant content", sessionID)
}
