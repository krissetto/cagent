package runtime

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/team"
)

func TestEventBus_SubscribePublishAndClose(t *testing.T) {
	bus := NewEventBus()
	sub := bus.Subscribe(t.Context(), "sess-1", 8)
	bus.Publish("sess-1", Warning("hello", "root"))

	select {
	case ev := <-sub.Events:
		warn, ok := ev.(*WarningEvent)
		require.True(t, ok)
		require.Equal(t, "hello", warn.Message)
	case <-time.After(time.Second):
		t.Fatal("expected published event")
	}

	bus.CloseTopic("sess-1")
	_, ok := <-sub.Events
	assert.False(t, ok, "subscription should close when topic closes")
}

func TestEventBus_SubscribeWithSnapshotReplaysInProgressTurn(t *testing.T) {
	t.Parallel()

	bus := NewEventBus()

	// Stream three deltas before any subscriber attaches. The bus is fan-out
	// only, so a normal Subscribe would lose them, but the per-topic snapshot
	// must accumulate them so a late attach can replay.
	bus.Publish("sess-1", AgentChoice("worker", "sess-1", "hello "))
	bus.Publish("sess-1", AgentChoice("worker", "sess-1", "there, "))
	bus.Publish("sess-1", AgentChoiceReasoning("worker", "sess-1", "thinking..."))

	sub, snapshot := bus.SubscribeWithSnapshot(t.Context(), "sess-1", 8)
	t.Cleanup(sub.Cancel)
	assert.True(t, snapshot.HasContent())
	assert.Equal(t, "worker", snapshot.AgentName)
	assert.Equal(t, "hello there, ", snapshot.Content)
	assert.Equal(t, "thinking...", snapshot.ReasoningContent)

	// A subsequent delta must be delivered to the subscription only and must
	// not appear duplicated in the snapshot view.
	bus.Publish("sess-1", AgentChoice("worker", "sess-1", "how can I help?"))
	select {
	case ev := <-sub.Events:
		ac, ok := ev.(*AgentChoiceEvent)
		require.True(t, ok)
		assert.Equal(t, "how can I help?", ac.Content,
			"the live subscription must only deliver the post-attach delta")
	case <-time.After(time.Second):
		t.Fatal("expected post-attach delta")
	}

	// Once the assistant message is committed, the snapshot must reset so the
	// next attach does not replay stale content from a previous turn.
	bus.Publish("sess-1", &MessageAddedEvent{Type: "message_added"})
	_, snapshot2 := bus.SubscribeWithSnapshot(t.Context(), "sess-1", 8)
	assert.False(t, snapshot2.HasContent(),
		"MessageAdded must reset the per-topic streaming snapshot")
}

func TestEventBus_SubscribeWithSnapshotIsAtomicWithSubscribe(t *testing.T) {
	t.Parallel()

	// This test validates the central claim of SubscribeWithSnapshot: there is
	// no overlap window where the same delta could appear in both the snapshot
	// and on the new subscription's channel.
	bus := NewEventBus()

	var wg sync.WaitGroup
	wg.Add(1)
	start := make(chan struct{})
	go func() {
		defer wg.Done()
		<-start
		for range 200 {
			bus.Publish("sess-x", AgentChoice("a", "sess-x", "x"))
		}
	}()
	close(start)

	// Race the subscription against the publisher. Whichever order wins, the
	// invariant must hold: snapshot.Content + concatenated subscription
	// deltas equals the full sequence of "x" deltas published.
	sub, snapshot := bus.SubscribeWithSnapshot(t.Context(), "sess-x", 256)
	t.Cleanup(sub.Cancel)
	wg.Wait()

	var collected strings.Builder
	collected.WriteString(snapshot.Content)
	deadline := time.After(time.Second)
	for {
		select {
		case ev := <-sub.Events:
			if ac, ok := ev.(*AgentChoiceEvent); ok {
				collected.WriteString(ac.Content)
			}
		case <-deadline:
			assert.Equal(t, strings.Repeat("x", 200), collected.String(),
				"snapshot + post-attach deltas must reconstruct the full streaming history exactly once")
			return
		}
	}
}

func TestLocalRuntime_SubscribeSessionReceivesRootEvents(t *testing.T) {
	stream := newStreamBuilder().AddContent("hello").AddStopWithUsage(5, 2).Build()
	prov := &mockProvider{id: "test/root", stream: stream}
	root := agent.New("root", "root", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	sess := session.New(session.WithUserMessage("hi"))
	sub := rt.SubscribeSession(ctx, sess.ID, 64)

	collected := make(chan []Event, 1)
	go func() {
		var got []Event
		for ev := range sub.Events {
			got = append(got, ev)
		}
		collected <- got
	}()

	for range rt.RunStream(ctx, sess) {
	}

	sub.Cancel()
	select {
	case events := <-collected:
		var sawChoice bool
		for _, ev := range events {
			if choice, ok := ev.(*AgentChoiceEvent); ok && choice.Content == "hello" {
				sawChoice = true
			}
		}
		assert.True(t, sawChoice, "observer should receive root session events")
	case <-time.After(2 * time.Second):
		t.Fatal("subscription collector did not finish")
	}
}

func TestLocalRuntime_SubscribeSessionReceivesChildObserverEvents(t *testing.T) {
	setup := newSubagentTestSetup(t)
	setup.parentP.streams = []chat.MessageStream{
		newStreamBuilder().
			AddToolCallName("tc1", subagent.ToolNameStart).
			AddToolCallArguments("tc1", `{"agent":"worker","task":"work"}`).
			AddStopWithUsage(10, 5).
			Build(),
		newStreamBuilder().AddContent("done").AddStopWithUsage(5, 2).Build(),
	}
	// The child loop launches a background title-generation goroutine at the
	// start of the child loop so title output can be ready before the first
	// turn completes. That goroutine shares the worker provider's FIFO stream
	// queue with the actual turns; either goroutine can pop the next stream.
	// We queue enough streams here that neither path can run the queue dry and
	// we keep the test assertions content-agnostic to avoid depending on
	// goroutine scheduling.
	setup.workerP.streams = []chat.MessageStream{
		newStreamBuilder().AddContent("turn-content-a").AddStopWithUsage(5, 2).Build(),
		newStreamBuilder().AddContent("turn-content-b").AddStopWithUsage(5, 2).Build(),
		newStreamBuilder().AddContent("spare-a").AddStopWithUsage(3, 1).Build(),
		newStreamBuilder().AddContent("spare-b").AddStopWithUsage(3, 1).Build(),
	}

	sess := session.New(session.WithUserMessage("delegate"), session.WithToolsApproved(true))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	var childID string
	events := setup.rt.RunStream(ctx, sess)
	for ev := range events {
		if started, ok := ev.(*SubAgentStartedEvent); ok {
			childID = started.SubAgent.ID
			break
		}
	}
	require.NotEmpty(t, childID)

	obs := setup.rt.SubscribeSession(ctx, childID, 64)
	defer obs.Cancel()
	require.NoError(t, setup.rt.subagents.Send(childID, subagent.Message{Content: "continue"}))

	deadline := time.After(2 * time.Second)
	var sawUser bool
	var agentChoiceCount int
Loop:
	for {
		select {
		case ev, ok := <-obs.Events:
			if !ok {
				break Loop
			}
			switch e := ev.(type) {
			case *UserMessageEvent:
				if e.Message == "continue" {
					sawUser = true
				}
			case *AgentChoiceEvent:
				// Title generation calls the provider directly (no event bus
				// publication), so every AgentChoice observed here comes from
				// an actual child turn's RunStream.
				agentChoiceCount++
			}
			if sawUser && agentChoiceCount >= 2 {
				break Loop
			}
		case <-deadline:
			break Loop
		}
	}

	assert.True(t, sawUser, "child observer should see parent->child message injection")
	assert.GreaterOrEqual(t, agentChoiceCount, 2,
		"child observer should see events from both child turns (first turn + continuation)")
}

func TestLocalRuntime_LiveSessionTreeAndNode(t *testing.T) {
	setup := newSubagentTestSetup(t)
	setup.workerP.streams = []chat.MessageStream{newStreamBuilder().AddContent("worker").AddStopWithUsage(5, 2).Build()}

	parent := session.New(session.WithUserMessage("root"), session.WithToolsApproved(true))
	childSess := setup.rt.newSubagentChildSession(parent, subagent.StartConfig{Parent: parent, AgentName: "worker", Task: "work", ToolsApproved: true, Title: "child"}, setup.worker)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	h, err := setup.rt.subagents.StartChild(ctx, subagent.StartConfig{Parent: parent, AgentName: "worker", Task: "work", ToolsApproved: true, Title: "child"}, childSess)
	require.NoError(t, err)

	tree := setup.rt.LiveSessionTree(parent.ID)
	require.Len(t, tree, 2)
	assert.Equal(t, parent.ID, tree[0].SessionID)
	assert.Equal(t, h.ID(), tree[1].SessionID)
	assert.Equal(t, 1, tree[1].Depth)

	node, ok := setup.rt.LiveSessionNode(h.ID())
	require.True(t, ok)
	assert.Equal(t, h.ID(), node.SessionID)
	assert.Equal(t, parent.ID, node.RootSessionID)
}

func TestLocalRuntime_LiveSessionNodeReflectsMirroredSubagentTitle(t *testing.T) {
	setup := newSubagentTestSetup(t)
	parent := session.New(session.WithUserMessage("root"), session.WithToolsApproved(true))
	childSess := setup.rt.newSubagentChildSession(parent, subagent.StartConfig{Parent: parent, AgentName: "worker", Task: "work", ToolsApproved: true, Title: ""}, setup.worker)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	h, err := setup.rt.subagents.StartChild(ctx, subagent.StartConfig{Parent: parent, AgentName: "worker", Task: "work", ToolsApproved: true, Title: ""}, childSess)
	require.NoError(t, err)

	require.NoError(t, setup.rt.subagents.SetTitle(h.ID(), "Mirrored title"))
	childSess.SetTitle("Mirrored title")

	node, ok := setup.rt.LiveSessionNode(h.ID())
	require.True(t, ok)
	assert.Equal(t, "Mirrored title", node.Title,
		"LiveSessionNode should reflect the handle title used by attached-tab seeding and sidebar snapshots")
}

func TestLocalRuntime_LiveSessionTreeIncludesPersistedDescendants(t *testing.T) {
	setup := newSubagentTestSetup(t)

	parent := session.New(session.WithUserMessage("root"), session.WithToolsApproved(true))

	// Simulate a persisted child session from a previous run: we add it
	// directly to the session store with parent_id set but do NOT register
	// it with the subagent.Manager.
	child := session.New(
		session.WithParentID(parent.ID),
		session.WithAgentName("worker"),
	)
	child.SetTitle("Persisted child")

	ctx := t.Context()
	require.NoError(t, setup.rt.sessionStore.AddSession(ctx, parent))
	require.NoError(t, setup.rt.sessionStore.AddSession(ctx, child))

	tree := setup.rt.LiveSessionTree(parent.ID)
	require.GreaterOrEqual(t, len(tree), 2, "tree should include the root plus the persisted child")

	var foundChild bool
	for _, node := range tree {
		if node.SessionID == child.ID {
			foundChild = true
			assert.Equal(t, "closed", node.Status,
				"persisted-only descendants should report status=closed")
			assert.Equal(t, "worker", node.AgentName)
			assert.Equal(t, "Persisted child", node.Title)
		}
	}
	assert.True(t, foundChild, "LiveSessionTree must include persisted descendants from the session store")
}

func TestLocalRuntime_LiveSessionNodeFallsBackToStore(t *testing.T) {
	setup := newSubagentTestSetup(t)

	parent := session.New(session.WithUserMessage("root"))
	child := session.New(
		session.WithParentID(parent.ID),
		session.WithAgentName("worker"),
	)

	ctx := t.Context()
	require.NoError(t, setup.rt.sessionStore.AddSession(ctx, parent))
	require.NoError(t, setup.rt.sessionStore.AddSession(ctx, child))

	node, ok := setup.rt.LiveSessionNode(child.ID)
	require.True(t, ok, "LiveSessionNode should resolve a persisted-only child session")
	assert.Equal(t, child.ID, node.SessionID)
	assert.Equal(t, "closed", node.Status)
	assert.Equal(t, "worker", node.AgentName)
}

func TestLocalRuntime_LiveSessionFallsBackToStore(t *testing.T) {
	setup := newSubagentTestSetup(t)

	parent := session.New(session.WithUserMessage("root"))
	child := session.New(
		session.WithParentID(parent.ID),
		session.WithAgentName("worker"),
		session.WithUserMessage("task for worker"),
	)

	ctx := t.Context()
	require.NoError(t, setup.rt.sessionStore.AddSession(ctx, parent))
	require.NoError(t, setup.rt.sessionStore.AddSession(ctx, child))

	sess, ok := setup.rt.LiveSession(child.ID)
	require.True(t, ok, "LiveSession should fall back to session store")
	assert.Equal(t, child.ID, sess.ID)
}

func TestLocalRuntime_FollowUpSessionByIDRejectsHistorical(t *testing.T) {
	setup := newSubagentTestSetup(t)

	parent := session.New(session.WithUserMessage("root"))
	child := session.New(
		session.WithParentID(parent.ID),
		session.WithAgentName("worker"),
	)

	ctx := t.Context()
	require.NoError(t, setup.rt.sessionStore.AddSession(ctx, parent))
	require.NoError(t, setup.rt.sessionStore.AddSession(ctx, child))

	err := setup.rt.FollowUpSessionByID(child.ID, QueuedMessage{Content: "hello"})
	assert.ErrorIs(t, err, ErrSessionNotInTree,
		"FollowUpSessionByID must reject historical sessions that aren't live in the manager")
}

func TestSteerSessionByID_RoutesToSteerInboxForChild(t *testing.T) {
	setup := newSubagentTestSetup(t)
	parent := session.New(session.WithUserMessage("root"), session.WithToolsApproved(true))
	childSess := setup.rt.newSubagentChildSession(parent, subagent.StartConfig{
		Parent:        parent,
		AgentName:     "worker",
		Task:          "work",
		ToolsApproved: true,
		Title:         "child",
	}, setup.worker)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	h, err := setup.rt.subagents.StartChild(ctx, subagent.StartConfig{
		Parent:        parent,
		AgentName:     "worker",
		Task:          "work",
		ToolsApproved: true,
		Title:         "child",
	}, childSess)
	require.NoError(t, err)

	require.True(t, setup.rt.subagents.WaitParentInbox(ctx, parent.ID))
	_ = setup.rt.subagents.DrainParentInbox(parent.ID)
	require.Eventually(t, func() bool {
		snap, e := setup.rt.subagents.Get(h.ID())
		return e == nil && snap.Status == subagent.StatusWaiting
	}, 2*time.Second, 20*time.Millisecond)

	require.NoError(t, setup.rt.SteerSessionByID(h.ID(), QueuedMessage{Content: "steer-now"}))

	steered := h.DrainSteerInbox()
	require.Len(t, steered, 1)
	assert.Equal(t, "steer-now", steered[0].Content)
	assert.Equal(t, subagent.MessageModeSteer, steered[0].Mode)
	assert.Empty(t, h.DrainInbox(), "steer messages must not land in the follow-up inbox")
}

func TestFollowUpSessionByID_RoutesToFollowupInboxForChild(t *testing.T) {
	setup := newSubagentTestSetup(t)
	parent := session.New(session.WithUserMessage("root"), session.WithToolsApproved(true))
	childSess := setup.rt.newSubagentChildSession(parent, subagent.StartConfig{
		Parent:        parent,
		AgentName:     "worker",
		Task:          "work",
		ToolsApproved: true,
		Title:         "child",
	}, setup.worker)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	h, err := setup.rt.subagents.StartChild(ctx, subagent.StartConfig{
		Parent:        parent,
		AgentName:     "worker",
		Task:          "work",
		ToolsApproved: true,
		Title:         "child",
	}, childSess)
	require.NoError(t, err)

	require.True(t, setup.rt.subagents.WaitParentInbox(ctx, parent.ID))
	_ = setup.rt.subagents.DrainParentInbox(parent.ID)
	require.Eventually(t, func() bool {
		snap, e := setup.rt.subagents.Get(h.ID())
		return e == nil && snap.Status == subagent.StatusWaiting
	}, 2*time.Second, 20*time.Millisecond)

	require.NoError(t, setup.rt.FollowUpSessionByID(h.ID(), QueuedMessage{Content: "follow-up"}))

	msgs := h.DrainInbox()
	require.Len(t, msgs, 1)
	assert.Equal(t, "follow-up", msgs[0].Content)
	assert.Equal(t, subagent.MessageModeFollowUp, msgs[0].Mode)
	assert.Empty(t, h.DrainSteerInbox(), "follow-up messages must not land in the steer inbox")
}

func TestSessionByID_RootRoutesToSessionQueues(t *testing.T) {
	// Use a blockingProvider so the root engine stays in a running turn while
	// we exercise SteerSessionByID/FollowUpSessionByID. Without it, fast mock
	// streams unregister the session from liveSessions before we get a chance
	// to call the routing APIs.
	prov := &blockingProvider{id: "test/root-block"}
	root := agent.New("root", "root", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	sess := session.New(session.WithUserMessage("hi"))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	events := rt.RunStream(ctx, sess)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range events {
		}
	}()

	require.Eventually(t, func() bool {
		_, ok := rt.liveSessions.get(sess.ID)
		return ok
	}, 2*time.Second, 20*time.Millisecond, "engine must register the live root session before steer/follow-up routing")

	require.NoError(t, rt.SteerSessionByID(sess.ID, QueuedMessage{Content: "steer root"}))
	require.NoError(t, rt.FollowUpSessionByID(sess.ID, QueuedMessage{Content: "follow root"}))

	steered := rt.steer.Drain(context.Background())
	require.Len(t, steered, 1)
	assert.Equal(t, "steer root", steered[0].Content)

	follow, ok := rt.followUp.Dequeue(context.Background())
	require.True(t, ok)
	assert.Equal(t, "follow root", follow.Content)

	cancel()
	<-done
}
