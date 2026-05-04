package runtime

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

// TestSubagentChildSession_IsNonInteractive verifies that subagent child
// sessions are marked NonInteractive so the engine's max-iterations gate
// auto-stops the loop instead of blocking forever on the runtime's
// resumeChan (which no one ever signals for a child loop).
func TestSubagentChildSession_IsNonInteractive(t *testing.T) {
	t.Parallel()

	setup := newSubagentTestSetup(t)
	parent := session.New(session.WithUserMessage("root"), session.WithToolsApproved(true))

	childSess := setup.rt.newSubagentChildSession(parent, subagent.StartConfig{
		Parent:        parent,
		AgentName:     "worker",
		Task:          "work",
		ToolsApproved: true,
		Title:         "child",
	}, setup.worker)

	assert.True(t, childSess.NonInteractive,
		"subagent child sessions must be NonInteractive so the engine's "+
			"max-iterations gate auto-stops instead of blocking on resumeChan")
}

// TestSubagentChildLoop_ClosesEventBusTopicOnExit verifies that when a
// subagent child loop exits, the runtime closes the child's event-bus
// topic so per-session topics + accumulated streaming snapshots do not
// leak. Subscribers should observe a graceful channel close after the
// terminal envelope has already been published.
func TestSubagentChildLoop_ClosesEventBusTopicOnExit(t *testing.T) {
	setup := newSubagentTestSetup(t)
	parent := session.New(session.WithUserMessage("root"), session.WithToolsApproved(true))
	setup.queueWorkerStream(t, newStreamBuilder().AddContent("hello").AddStopWithUsage(5, 2))

	// Pre-set a non-empty Title so generateSubagentTitle is skipped entirely
	// (it short-circuits when sess.GetTitle() != ""). This avoids racing the
	// title-generation goroutine with the topic-close defer for this test;
	// that race is a separate, pre-existing concern outside the scope of
	// this test.
	cfg := subagent.StartConfig{
		Parent:        parent,
		AgentName:     "worker",
		Task:          "work",
		ToolsApproved: true,
		Title:         "child",
	}
	childSess := setup.rt.newSubagentChildSession(parent, cfg, setup.worker)
	childID := childSess.ID

	ctx, cancel := timeoutCtx(t, 5*time.Second)
	defer cancel()

	// Subscribe before the child starts so we observe the channel close.
	sub := setup.rt.SubscribeSession(ctx, childID, 64)
	t.Cleanup(sub.Cancel)

	h, err := setup.rt.subagents.StartChild(ctx, cfg, childSess)
	require.NoError(t, err)

	// Wait for the first turn so we know the loop is fully running.
	require.True(t, setup.rt.subagents.WaitParentInbox(ctx, parent.ID))
	_ = setup.rt.subagents.DrainParentInbox(parent.ID)

	// Ask the child to close, then wait for the loop's done channel. After
	// done closes, every defer in the loop goroutine has already run - in
	// particular CloseTopic for h.ID().
	require.NoError(t, setup.rt.subagents.Close(h.ID()))
	loopDone := h.LoopDone()
	require.NotNil(t, loopDone, "manager must wire the child loop done channel")
	select {
	case <-loopDone:
	case <-time.After(3 * time.Second):
		t.Fatal("child loop did not exit after Close")
	}

	// The subscription channel must drain and close within a short window
	// after the loop's defer chain ran.
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	closed := false
	for !closed {
		select {
		case _, ok := <-sub.Events:
			if !ok {
				closed = true
			}
		case <-timer.C:
			t.Fatal("subscription channel was not closed after child loop exit; CloseTopic did not fire")
		}
	}

	// And the topic must be removed from ActiveTopics so a memory snapshot
	// would not show a leaked per-session topic.
	for _, id := range setup.rt.eventBus.ActiveTopics() {
		assert.NotEqual(t, childID, id,
			"child session's event bus topic must be removed from the bus after loop exit")
	}
}
