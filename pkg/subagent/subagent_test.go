package subagent

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/inbox"
	"github.com/docker/docker-agent/pkg/session"
)

type fakeRunner struct {
	start func(context.Context, *Handle) <-chan struct{}
}

func (f fakeRunner) StartChildLoop(ctx context.Context, h *Handle) <-chan struct{} {
	if f.start != nil {
		return f.start(ctx, h)
	}
	done := make(chan struct{})
	close(done)
	return done
}

func TestTruncatePreview(t *testing.T) {
	preview, truncated := TruncatePreview("hello\n\n  world", 100)
	require.Equal(t, "hello world", preview)
	require.False(t, truncated)

	preview, truncated = TruncatePreview("abcdef", 4)
	require.Equal(t, "abc…", preview)
	require.True(t, truncated)
}

func TestFormatEnvelopeMessage(t *testing.T) {
	msg := FormatEnvelopeMessage(Envelope{
		SubAgentID: "child-1",
		AgentName:  "worker",
		Kind:       UpdateKindTurnCompleted,
		Status:     StatusWaiting,
		Preview:    "done",
	})
	assert.Contains(t, msg, "subagent_id: "+ShortRef("child-1"))
	assert.Contains(t, msg, "agent: worker")
	assert.Contains(t, msg, "kind: turn_completed")
	assert.Contains(t, msg, "preview: done")
}

func TestManagerSendRoutingByMode(t *testing.T) {
	t.Parallel()

	parent := session.New(session.WithUserMessage("hello"))
	mgr := NewManager(fakeRunner{})
	child := session.New(session.WithAgentName("worker"))
	h, err := mgr.StartChild(t.Context(), StartConfig{
		Parent:    parent,
		AgentName: "worker",
		Task:      "do it",
	}, child)
	require.NoError(t, err)

	// Followup (zero/empty mode) goes to regular inbox.
	require.NoError(t, mgr.Send(h.ID(), Message{Content: "followup"}))
	assert.True(t, h.HasPendingInbox(), "followup message should make HasPendingInbox true")
	assert.Empty(t, h.DrainSteerInbox(), "followup must not appear in steer inbox")
	require.Len(t, h.DrainInbox(), 1)

	// Steer mode goes to steer inbox only.
	require.NoError(t, mgr.Send(h.ID(), Message{Content: "steer", Mode: MessageModeSteer}))
	assert.True(t, h.HasPendingInbox(), "steer message should make HasPendingInbox true (both queues)")
	assert.Empty(t, h.DrainInbox(), "steer must not appear in followup inbox")
	steered := h.DrainSteerInbox()
	require.Len(t, steered, 1)
	assert.Equal(t, "steer", steered[0].Content)
	assert.False(t, h.HasPendingInbox(), "both inboxes drained, HasPendingInbox must be false")
}

func TestManagerStartSendDrainAndWait(t *testing.T) {
	parent := session.New(session.WithUserMessage("hello"))
	var started *Handle
	mgr := NewManager(fakeRunner{start: func(_ context.Context, h *Handle) <-chan struct{} {
		started = h
		done := make(chan struct{})
		close(done)
		return done
	}})

	child := session.New(session.WithAgentName("worker"))
	h, err := mgr.StartChild(t.Context(), StartConfig{
		Parent:    parent,
		AgentName: "worker",
		Task:      "do it",
	}, child)
	require.NoError(t, err)
	require.NotNil(t, h)
	require.Equal(t, h, started)

	// Parent -> child follow-up (default/zero mode).
	require.NoError(t, mgr.Send(h.ID(), Message{Content: "follow up"}))
	msgs := h.DrainInbox()
	require.Len(t, msgs, 1)
	require.Equal(t, "follow up", msgs[0].Content)
	require.Equal(t, MessageModeFollowUp, msgs[0].Mode) // zero value
	// Steer mode routes to the steer inbox, not the regular inbox.
	require.NoError(t, mgr.Send(h.ID(), Message{Content: "urgent", Mode: MessageModeSteer}))
	require.Empty(t, h.DrainInbox(), "steer msg must not appear in follow-up inbox")
	steered := h.DrainSteerInbox()
	require.Len(t, steered, 1)
	require.Equal(t, "urgent", steered[0].Content)
	require.Equal(t, MessageModeSteer, steered[0].Mode)

	// Child -> parent.
	h.PublishTurn("all done")
	require.True(t, mgr.WaitParentInbox(t.Context(), parent.ID))
	envs := mgr.DrainParentInbox(parent.ID)
	require.Len(t, envs, 1)
	require.Equal(t, UpdateKindTurnCompleted, envs[0].Kind)
	require.Equal(t, "all done", envs[0].Preview)
}

func TestManagerMarkWaitingSilentlyPublishesStatusOnlyWithoutParentInbox(t *testing.T) {
	t.Parallel()

	parent := session.New(session.WithUserMessage("hello"))
	mgr := NewManager(fakeRunner{})
	child := session.New(session.WithAgentName("worker"))
	h, err := mgr.StartChild(t.Context(), StartConfig{
		Parent:    parent,
		AgentName: "worker",
		Task:      "do it",
	}, child)
	require.NoError(t, err)

	var observed []Envelope
	mgr.AddEnvelopePublishedListener(func(env Envelope) {
		observed = append(observed, env)
	})

	h.MarkRunning()
	h.MarkWaitingSilently()

	require.Equal(t, StatusWaiting, h.Status())
	require.Len(t, observed, 1, "status-only update should still reach envelope listeners for EventBus fanout")
	assert.Equal(t, UpdateKindStatusOnly, observed[0].Kind)
	assert.Equal(t, StatusWaiting, observed[0].Status)
	assert.Equal(t, h.ID(), observed[0].SubAgentID)

	assert.Empty(t, mgr.DrainParentInbox(parent.ID),
		"status-only updates must not wake the parent loop through its inbox")
	assert.True(t, mgr.HasInFlightChildren(parent.ID),
		"parked interrupted child should keep the parent waiting for a future real envelope")
}

func TestManagerHasInFlightChildrenIncludesParkedInterruptedChild(t *testing.T) {
	t.Parallel()

	parent := session.New(session.WithUserMessage("hello"))
	mgr := NewManager(fakeRunner{})
	child := session.New(session.WithAgentName("worker"))
	h, err := mgr.StartChild(t.Context(), StartConfig{
		Parent:    parent,
		AgentName: "worker",
		Task:      "do it",
	}, child)
	require.NoError(t, err)

	h.MarkRunning()
	require.True(t, mgr.HasInFlightChildren(parent.ID))

	// MarkWaitingSilently models ESC in an attached child tab: the current
	// child turn is interrupted and the parent gets a status-only UI update,
	// but the child loop is still parked and may subsequently publish a real
	// turn-completed envelope. The parent must keep waiting so it can consume
	// that future envelope.
	h.MarkWaitingSilently()
	require.Equal(t, StatusWaiting, h.Status())
	require.True(t, mgr.HasInFlightChildren(parent.ID),
		"parked interrupted child must keep parent waitForSubagentInbox alive")

	// Once the child either starts a new turn or publishes a normal turn, the
	// parked marker is cleared. PublishTurn also queues an envelope, so the
	// parent remains in-flight until it drains that envelope.
	h.PublishTurn("done after interrupt")
	require.True(t, mgr.HasInFlightChildren(parent.ID),
		"queued turn-completed envelope should keep parent in-flight")
	require.NotEmpty(t, mgr.DrainParentInbox(parent.ID))
	require.False(t, mgr.HasInFlightChildren(parent.ID),
		"after consuming the real envelope, waiting child should no longer be in-flight")
}

func TestManagerHasInFlightChildrenExcludesWaiting(t *testing.T) {
	parent := session.New(session.WithUserMessage("hello"))
	mgr := NewManager(fakeRunner{})
	child := session.New(session.WithAgentName("worker"))
	h, err := mgr.StartChild(t.Context(), StartConfig{
		Parent:    parent,
		AgentName: "worker",
		Task:      "do it",
	}, child)
	require.NoError(t, err)

	h.MarkRunning()
	require.True(t, mgr.HasInFlightChildren(parent.ID))

	h.PublishTurn("done for now")
	// PublishTurn queues an envelope for the parent; that envelope itself
	// counts as in-flight until drained.
	require.True(t, mgr.HasInFlightChildren(parent.ID))
	require.NotEmpty(t, mgr.DrainParentInbox(parent.ID))
	require.False(t, mgr.HasInFlightChildren(parent.ID))
	require.True(t, mgr.HasLiveChildren(parent.ID))
}

func TestManagerHasInFlightChildrenCountsPendingParentEnvelope(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		publish func(*Handle)
	}{
		{
			name: "waiting child",
			publish: func(h *Handle) {
				h.PublishTurn("done for now")
			},
		},
		{
			name: "terminal child",
			publish: func(h *Handle) {
				h.PublishStopped()
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent := session.New(session.WithUserMessage("hello"))
			mgr := NewManager(fakeRunner{})
			child := session.New(session.WithAgentName("worker"))
			h, err := mgr.StartChild(t.Context(), StartConfig{
				Parent:    parent,
				AgentName: "worker",
				Task:      "do it",
			}, child)
			require.NoError(t, err)

			tc.publish(h)

			require.True(t, mgr.HasInFlightChildren(parent.ID),
				"queued envelope for root must count as in-flight until drained")

			envs := mgr.DrainParentInbox(parent.ID)
			require.Len(t, envs, 1)
			require.False(t, mgr.HasInFlightChildren(parent.ID),
				"once the queued envelope is drained, child should no longer count as in-flight")
		})
	}
}

func TestManagerHasInFlightChildrenCountsWaitingChildWithPendingGrandchildEnvelope(t *testing.T) {
	t.Parallel()

	root := session.New(session.WithUserMessage("root"))
	mgr := NewManager(fakeRunner{})

	child := session.New(session.WithAgentName("worker"))
	hChild, err := mgr.StartChild(t.Context(), StartConfig{
		Parent:    root,
		AgentName: "worker",
		Task:      "child",
	}, child)
	require.NoError(t, err)

	grandchild := session.New(session.WithAgentName("leaf"))
	hGrand, err := mgr.StartChild(t.Context(), StartConfig{
		Parent:    child,
		AgentName: "leaf",
		Task:      "grandchild",
	}, grandchild)
	require.NoError(t, err)

	// Move both child and grandchild into the waiting state. Each PublishTurn
	// emits one envelope into its respective parent inbox.
	hChild.PublishTurn("child waiting")
	hGrand.PublishTurn("grandchild waiting")

	// Drain root's own inbox so the queued root envelope no longer counts as
	// in-flight on its own. The grandchild envelope queued in the child's
	// parent inbox stays.
	require.NotEmpty(t, mgr.DrainParentInbox(root.ID))

	// Even though every descendant is in StatusWaiting and root has nothing
	// queued, the pending grandchild envelope in the child's mailbox means the
	// child can still wake and propagate work upward to root.
	require.True(t, mgr.HasInFlightChildren(root.ID),
		"waiting child with pending grandchild envelope must still count as in-flight")

	// Once that envelope is drained from the child's mailbox, root finally goes
	// idle.
	require.NotEmpty(t, mgr.DrainParentInbox(child.ID))
	require.False(t, mgr.HasInFlightChildren(root.ID),
		"after child drains its grandchild envelope, root should be idle")
}

// TestManagerHasInFlightChildrenIgnoresGrandchildBehindTerminalChild verifies
// that a running grandchild behind a terminal direct child does NOT keep the
// root "in flight". The terminal child's envelope delivery path is dead, so
// the grandchild can never wake the root's inbox.
func TestManagerHasInFlightChildrenIgnoresGrandchildBehindTerminalChild(t *testing.T) {
	t.Parallel()

	root := session.New(session.WithUserMessage("root"))
	mgr := NewManager(fakeRunner{})

	child := session.New(session.WithAgentName("worker"))
	hChild, err := mgr.StartChild(t.Context(), StartConfig{
		Parent:    root,
		AgentName: "worker",
		Task:      "a",
	}, child)
	require.NoError(t, err)

	grandchild := session.New(session.WithAgentName("leaf"))
	hGrand, err := mgr.StartChild(t.Context(), StartConfig{
		Parent:    child,
		AgentName: "leaf",
		Task:      "b",
	}, grandchild)
	require.NoError(t, err)

	// Grandchild is running.
	hGrand.MarkRunning()

	// While child is also live, root should see in-flight (grandchild
	// reachable through waiting child).
	hChild.PublishTurn("waiting")
	require.True(t, mgr.HasInFlightChildren(root.ID),
		"running grandchild reachable through waiting child should be in-flight")
	require.NotEmpty(t, mgr.DrainParentInbox(root.ID))

	// Now terminate the child. Grandchild is still running, but unreachable
	// from root's perspective — the envelope delivery path is dead.
	hChild.PublishStopped()
	// The terminal envelope itself is a queued root envelope and counts as
	// in-flight until drained; after the parent loop consumes it, the
	// running grandchild behind a terminal child must not keep root in-flight.
	require.NotEmpty(t, mgr.DrainParentInbox(root.ID))
	require.False(t, mgr.HasInFlightChildren(root.ID),
		"running grandchild behind terminal child must not keep root in-flight")
}

func TestManagerDepthAndAncestors(t *testing.T) {
	root := session.New()
	mgr := NewManager(fakeRunner{})

	child1 := session.New(session.WithAgentName("worker"))
	h1, err := mgr.StartChild(t.Context(), StartConfig{Parent: root, AgentName: "worker", Task: "a"}, child1)
	require.NoError(t, err)
	require.Equal(t, 1, h1.Depth())

	child2 := session.New(session.WithAgentName("leaf"))
	h2, err := mgr.StartChild(t.Context(), StartConfig{Parent: child1, AgentName: "leaf", Task: "b"}, child2)
	require.NoError(t, err)
	require.Equal(t, 2, h2.Depth())
	require.Equal(t, []string{child1.ID, root.ID}, mgr.Ancestors(h2.ID()))
}

func TestManagerDepthLimit(t *testing.T) {
	root := session.New()
	mgr := NewManager(fakeRunner{}, WithMaxDepth(1))

	child1 := session.New(session.WithAgentName("worker"))
	_, err := mgr.StartChild(t.Context(), StartConfig{Parent: root, AgentName: "worker", Task: "a"}, child1)
	require.NoError(t, err)

	child2 := session.New(session.WithAgentName("leaf"))
	_, err = mgr.StartChild(t.Context(), StartConfig{Parent: child1, AgentName: "leaf", Task: "b"}, child2)
	var depthErr *DepthExceededError
	assert.ErrorAs(t, err, &depthErr)
}

func TestManagerDescendantLimit(t *testing.T) {
	root := session.New()
	mgr := NewManager(fakeRunner{}, WithMaxDescendants(1))

	child1 := session.New(session.WithAgentName("worker"))
	_, err := mgr.StartChild(t.Context(), StartConfig{Parent: root, AgentName: "worker", Task: "a"}, child1)
	require.NoError(t, err)

	child2 := session.New(session.WithAgentName("leaf"))
	_, err = mgr.StartChild(t.Context(), StartConfig{Parent: root, AgentName: "leaf", Task: "b"}, child2)
	var limitErr *DescendantLimitError
	assert.ErrorAs(t, err, &limitErr)
}

func TestManagerIdleAutoFinalizeOption(t *testing.T) {
	t.Parallel()

	mgr := NewManager(fakeRunner{}, WithIdleAutoFinalize(2*time.Minute))
	defer mgr.Shutdown()
	assert.Equal(t, 2*time.Minute, mgr.IdleAutoFinalizeTimeout())
}

func TestManagerSweepIdleFinalizesWaitingChildren(t *testing.T) {
	t.Parallel()

	parent := session.New(session.WithUserMessage("hello"))
	mgr := NewManager(fakeRunner{})
	child := session.New(session.WithAgentName("worker"))
	h, err := mgr.StartChild(t.Context(), StartConfig{
		Parent:    parent,
		AgentName: "worker",
		Task:      "do it",
	}, child)
	require.NoError(t, err)

	// PublishTurn moves the handle to waiting and stamps LastUpdateAt.
	h.PublishTurn("done for now")

	// Force the handle to appear stale and sweep.
	h.lastUpdateAt.Store(time.Now().Add(-5 * time.Minute))
	mgr.sweepIdle(time.Minute)

	select {
	case <-h.CloseCh():
	case <-time.After(time.Second):
		t.Fatal("expected idle sweep to request finalize on stale waiting subagent")
	}
}

func TestManagerSweepIdleSkipsRunningAndFreshChildren(t *testing.T) {
	t.Parallel()

	parent := session.New(session.WithUserMessage("hello"))
	mgr := NewManager(fakeRunner{})

	freshChild := session.New(session.WithAgentName("fresh"))
	fresh, err := mgr.StartChild(t.Context(), StartConfig{Parent: parent, AgentName: "fresh", Task: "a"}, freshChild)
	require.NoError(t, err)
	fresh.PublishTurn("fresh")
	fresh.lastUpdateAt.Store(time.Now())

	runningChild := session.New(session.WithAgentName("runner"))
	running, err := mgr.StartChild(t.Context(), StartConfig{Parent: parent, AgentName: "runner", Task: "b"}, runningChild)
	require.NoError(t, err)
	running.MarkRunning()

	mgr.sweepIdle(time.Minute)

	select {
	case <-fresh.CloseCh():
		t.Fatal("fresh waiting child should not be finalized")
	default:
	}
	select {
	case <-running.CloseCh():
		t.Fatal("running child should not be finalized")
	default:
	}
}

func TestManagerCascadeStopStopsDescendants(t *testing.T) {
	root := session.New()
	mgr := NewManager(fakeRunner{})

	child1 := session.New(session.WithAgentName("worker"))
	h1, err := mgr.StartChild(t.Context(), StartConfig{Parent: root, AgentName: "worker", Task: "a"}, child1)
	require.NoError(t, err)
	child2 := session.New(session.WithAgentName("leaf"))
	h2, err := mgr.StartChild(t.Context(), StartConfig{Parent: child1, AgentName: "leaf", Task: "b"}, child2)
	require.NoError(t, err)

	mgr.CascadeStop(h1.ID())
	select {
	case <-h1.CloseCh():
	case <-time.After(time.Second):
		t.Fatal("expected direct child close channel to close")
	}
	select {
	case <-h2.CloseCh():
	case <-time.After(time.Second):
		t.Fatal("expected descendant close channel to close")
	}
}

func TestManagerParentInboxSignal(t *testing.T) {
	parent := session.New()
	mgr := NewManager(fakeRunner{})
	child := session.New(session.WithAgentName("worker"))
	h, err := mgr.StartChild(t.Context(), StartConfig{Parent: parent, AgentName: "worker", Task: "a"}, child)
	require.NoError(t, err)

	signal := mgr.ParentInboxSignal(parent.ID)
	h.PublishTurn("done")
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatal("expected parent inbox signal to fire")
	}
}

func TestManagerInterruptCancelsCurrentTurn(t *testing.T) {
	t.Parallel()

	parent := session.New(session.WithUserMessage("hello"))
	mgr := NewManager(fakeRunner{})
	child := session.New(session.WithAgentName("worker"))
	h, err := mgr.StartChild(t.Context(), StartConfig{
		Parent:    parent,
		AgentName: "worker",
		Task:      "do it",
	}, child)
	require.NoError(t, err)

	// Simulate the child loop arming a per-turn cancel.
	turnCtx, turnCancel := context.WithCancel(t.Context())
	h.SetInterruptCancel(turnCancel)

	require.NoError(t, mgr.Interrupt(h.ID()))

	select {
	case <-turnCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("Interrupt should cancel the armed per-turn context")
	}

	// The subagent remains live afterwards: Interrupt does not cascade or
	// flip the handle to a terminal state.
	require.True(t, h.IsLive(), "Interrupt must not terminate the subagent")
}

func TestManagerInterruptUnknownAndTerminal(t *testing.T) {
	t.Parallel()

	mgr := NewManager(fakeRunner{})

	var nfErr *NotFoundError
	require.ErrorAs(t, mgr.Interrupt("does-not-exist"), &nfErr)

	parent := session.New(session.WithUserMessage("hello"))
	child := session.New(session.WithAgentName("worker"))
	h, err := mgr.StartChild(t.Context(), StartConfig{
		Parent:    parent,
		AgentName: "worker",
		Task:      "do it",
	}, child)
	require.NoError(t, err)

	// Move the handle to a terminal state and confirm Interrupt rejects it
	// with a ClosedError rather than silently succeeding.
	h.PublishStopped()
	var closedErr *ClosedError
	require.ErrorAs(t, mgr.Interrupt(h.ID()), &closedErr)
}

func TestManagerInterruptWithoutArmedTurnIsNoop(t *testing.T) {
	t.Parallel()

	parent := session.New(session.WithUserMessage("hello"))
	mgr := NewManager(fakeRunner{})
	child := session.New(session.WithAgentName("worker"))
	h, err := mgr.StartChild(t.Context(), StartConfig{
		Parent:    parent,
		AgentName: "worker",
		Task:      "do it",
	}, child)
	require.NoError(t, err)

	// No SetInterruptCancel has been called — Interrupt should not panic and
	// must not terminate the subagent.
	require.NoError(t, mgr.Interrupt(h.ID()))
	require.True(t, h.IsLive())
}

func TestManagerStopAllCancelsLiveChildren(t *testing.T) {
	t.Parallel()

	parent := session.New(session.WithUserMessage("hello"))
	started := make(chan struct{}, 2)
	mgr := NewManager(fakeRunner{start: func(ctx context.Context, h *Handle) <-chan struct{} {
		done := make(chan struct{})
		go func() {
			defer close(done)
			select {
			case started <- struct{}{}:
			default:
			}
			<-ctx.Done()
			h.PublishStopped()
		}()
		return done
	}})

	for range 2 {
		child := session.New(session.WithAgentName("worker"))
		_, err := mgr.StartChild(t.Context(), StartConfig{Parent: parent, AgentName: "worker", Task: "do it"}, child)
		require.NoError(t, err)
	}

	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("expected child loop to start")
		}
	}

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	require.NoError(t, mgr.StopAll(ctx))

	for _, snap := range mgr.ListParent(parent.ID) {
		assert.True(t, snap.Status.IsTerminal(), "all children must be terminal after StopAll")
	}
}

func TestManagerAppendParentEnvelopeBoundsAndCoalesces(t *testing.T) {
	t.Parallel()

	var envs []Envelope
	for range parentEnvelopeCap + 10 {
		envs = appendParentEnvelope(envs, Envelope{
			SubAgentID: "child-a",
			Kind:       UpdateKindTurnCompleted,
			Preview:    "preview-a",
		})
		// Interleave another child so same-child consecutive coalescing is tested
		// without collapsing the whole stream down to one element.
		envs = appendParentEnvelope(envs, Envelope{
			SubAgentID: "child-b",
			Kind:       UpdateKindTurnCompleted,
			Preview:    "preview-b",
		})
	}
	// Add terminal envelopes near the end; these must survive trimming.
	envs = appendParentEnvelope(envs, Envelope{SubAgentID: "child-a", Kind: UpdateKindClosed, Preview: "closed"})
	envs = appendParentEnvelope(envs, Envelope{SubAgentID: "child-b", Kind: UpdateKindFailed, Preview: "failed"})

	require.LessOrEqual(t, len(envs), parentEnvelopeCap)
	assert.Equal(t, UpdateKindClosed, envs[len(envs)-2].Kind)
	assert.Equal(t, UpdateKindFailed, envs[len(envs)-1].Kind)

	// Consecutive turn-completed updates from the same subagent should coalesce
	// to the latest preview.
	envs = appendParentEnvelope(envs, Envelope{SubAgentID: "child-c", Kind: UpdateKindTurnCompleted, Preview: "old"})
	envs = appendParentEnvelope(envs, Envelope{SubAgentID: "child-c", Kind: UpdateKindTurnCompleted, Preview: "new"})
	assert.Equal(t, "new", envs[len(envs)-1].Preview)
}

func TestManagerDrainParentInboxConsumesStaleSignal(t *testing.T) {
	t.Parallel()

	mgr := NewManager(fakeRunner{})
	parent := session.New(session.WithUserMessage("root"))
	signal := mgr.ParentInboxSignal(parent.ID)

	mgr.PublishEnvelope(Envelope{
		SubAgentID:      "child-1",
		ParentSessionID: parent.ID,
		AgentName:       "worker",
		Kind:            UpdateKindTurnCompleted,
		Status:          StatusWaiting,
		Preview:         "done",
	})

	envs := mgr.DrainParentInbox(parent.ID)
	require.Len(t, envs, 1)

	// Draining the inbox must also consume the notify tick. Otherwise a later
	// select on ParentInboxSignal can wake spuriously even though the inbox is
	// empty, which in turn can trigger another model call with the conversation
	// still ending on an assistant message.
	select {
	case <-signal:
		t.Fatal("expected drained parent inbox to leave no stale signal behind")
	default:
	}
}

func TestMessageQueueDrainConsumesStaleSignal(t *testing.T) {
	t.Parallel()

	q := inbox.NewUnboundedQueue[Message]()
	signal := q.Signal()
	require.True(t, q.Push(Message{Content: "hi"}))

	msgs := q.Drain()
	require.Len(t, msgs, 1)

	// Drain must also consume the notify tick so a later select on the
	// queue's signal channel does not wake spuriously when the queue is
	// already empty. Without this, the StartChildLoop's outer select can
	// fire on a stale tick, drain zero messages, and then start another
	// turn with the conversation still ending on an assistant message.
	select {
	case <-signal:
		t.Fatal("expected drained queue to leave no stale signal behind")
	default:
	}
}

func TestManagerCloseAndStop(t *testing.T) {
	parent := session.New(session.WithUserMessage("hello"))
	mgr := NewManager(fakeRunner{})
	child := session.New(session.WithAgentName("worker"))
	h, err := mgr.StartChild(t.Context(), StartConfig{
		Parent:    parent,
		AgentName: "worker",
		Task:      "do it",
	}, child)
	require.NoError(t, err)

	require.NoError(t, mgr.Close(h.ID()))
	select {
	case <-h.CloseCh():
	case <-time.After(time.Second):
		t.Fatal("expected close channel to close")
	}

	// Once terminal, Send should fail with ErrClosed.
	h.PublishClosed()
	err = mgr.Send(h.ID(), Message{Content: "late"})
	var closedErr *ClosedError
	assert.ErrorAs(t, err, &closedErr)
}
