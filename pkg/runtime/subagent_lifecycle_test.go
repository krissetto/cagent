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
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/team"
)

// TestSubagent_NaturalExitPublishesTerminalEnvelope is a regression test for
// the bug where child loops only published a terminal envelope when the
// engine exited via context cancellation, an ErrorEvent, or an explicit close
// request. The non-interactive max-iterations auto-stop path returns
// outcomeDone with no ErrorEvent and no context cancellation, so the child
// loop used to exit silently and leak the handle in a non-terminal state.
//
// The fix makes StartChildLoop's terminal-envelope classification always emit
// a terminal envelope (PublishStopped) for any clean engine exit. This test
// drives the worker agent against a tight MaxIterations cap, drains all the
// per-turn envelopes, and asserts that the final envelope is a terminal
// Stopped/Closed/Failed envelope and the handle reports a terminal status.
func TestSubagent_NaturalExitPublishesTerminalEnvelope(t *testing.T) {
	t.Parallel()

	// Build a minimal team where the worker has MaxIterations=2. With each
	// completed turn incrementing sessionEngine.iteration before the next
	// runOneTurn call, the third runOneTurn entry hits the cap and the
	// non-interactive branch returns outcomeDone with nil err and no
	// ErrorEvent — the exact silent-exit path we want to assert on.
	parentP := &queueProvider{id: "test/parent"}
	workerP := &queueProvider{id: "test/worker"}
	worker := agent.New("worker", "Worker.",
		agent.WithModel(workerP),
		agent.WithMaxIterations(2),
	)
	parent := agent.New("root", "Root.",
		agent.WithModel(parentP),
		agent.WithToolSets(subagent.NewToolSet()),
	)
	agent.WithSubAgents(worker)(parent)

	tm := team.New(team.WithAgents(parent, worker))
	rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	parentSess := session.New(session.WithUserMessage("root"), session.WithToolsApproved(true))

	// Two turns of plain assistant content. The third runOneTurn invocation
	// hits the iteration cap before doing any model call, so we don't queue
	// a third stream. Title generation runs concurrently against the same
	// queue, so duplicate the first response to keep the test
	// schedule-independent.
	workerP.streams = []chat.MessageStream{
		newStreamBuilder().AddContent("turn1").AddStopWithUsage(5, 2).Build(),
		newStreamBuilder().AddContent("turn1").AddStopWithUsage(5, 2).Build(),
		newStreamBuilder().AddContent("turn2").AddStopWithUsage(5, 2).Build(),
	}

	childSess := rt.newSubagentChildSession(parentSess, subagent.StartConfig{
		Parent:        parentSess,
		AgentName:     "worker",
		Task:          "iterate",
		ToolsApproved: true,
		Title:         "child",
	}, worker)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	h, err := rt.subagents.StartChild(ctx, subagent.StartConfig{
		Parent:        parentSess,
		AgentName:     "worker",
		Task:          "iterate",
		ToolsApproved: true,
		Title:         "child",
	}, childSess)
	require.NoError(t, err)
	defer func() { _ = rt.subagents.Stop(h.ID()) }()

	// First turn completes — drain the per-turn envelope, then send a
	// follow-up to drive a second turn.
	require.True(t, rt.subagents.WaitParentInbox(ctx, parentSess.ID))
	envs := rt.subagents.DrainParentInbox(parentSess.ID)
	require.NotEmpty(t, envs)
	require.Equal(t, subagent.UpdateKindTurnCompleted, envs[len(envs)-1].Kind)

	require.NoError(t, rt.subagents.Send(h.ID(), subagent.Message{Content: "again"}))

	// Second turn completes — drain again.
	require.True(t, rt.subagents.WaitParentInbox(ctx, parentSess.ID))
	envs = rt.subagents.DrainParentInbox(parentSess.ID)
	require.NotEmpty(t, envs)
	require.Equal(t, subagent.UpdateKindTurnCompleted, envs[len(envs)-1].Kind)

	// Drive the third entry into runOneTurn, which is the one that hits the
	// MaxIterations cap and exits via the non-interactive auto-stop path.
	require.NoError(t, rt.subagents.Send(h.ID(), subagent.Message{Content: "again"}))

	// We must observe a terminal envelope from the natural exit. Before the
	// fix, this drain returned nothing because the child loop teardown fell
	// through the clean-exit path without publishing a terminal envelope.
	require.True(t, rt.subagents.WaitParentInbox(ctx, parentSess.ID),
		"natural max-iterations exit must publish a terminal envelope")
	envs = rt.subagents.DrainParentInbox(parentSess.ID)
	require.NotEmpty(t, envs)

	// The final envelope must be a terminal kind. PublishStopped is the
	// expected one for the natural-exit branch we just exercised, but we
	// accept any terminal status to keep the test resilient to future
	// classification refinements.
	terminal := envs[len(envs)-1]
	assert.True(t, terminal.Status.IsTerminal(),
		"final envelope status must be terminal, got %s", terminal.Status)
	assert.Contains(t,
		[]subagent.UpdateKind{
			subagent.UpdateKindStopped,
			subagent.UpdateKindClosed,
			subagent.UpdateKindFailed,
		},
		terminal.Kind,
		"final envelope kind must be a terminal lifecycle update")

	// The handle itself must report terminal status to the manager so the
	// parent's HasInFlightChildren no longer parks waiting for it.
	require.Eventually(t, func() bool {
		snap, err := rt.subagents.Get(h.ID())
		return err == nil && snap.Status.IsTerminal()
	}, 2*time.Second, 20*time.Millisecond,
		"manager snapshot must transition to terminal status after natural exit")

	assert.False(t, rt.subagents.HasInFlightChildren(parentSess.ID),
		"parent must no longer see in-flight children after natural exit")
}

// TestSubagent_WakeNextReParksOnStaleInboxSignal is a regression test for
// the P0 wake bug where childWakePolicy.wakeNext unconditionally returned true
// after a select case fired, even when the corresponding Drain returned zero
// items. A coalesced or stale single-slot signal on the grandchild inbox or
// direct parent→child inbox channel would cause the child to run an extra
// model turn with the conversation still ending on an assistant message.
// Providers like Anthropic reject that shape as unsupported assistant prefill.
//
// The fix wraps the select in a re-parking loop: the policy only calls
// MarkRunning + returns true when real work was injected.
//
// This is a direct unit test of childWakePolicy.wakeNext: we pre-fire a
// controllable signal channel while the corresponding drain is empty, then
// verify wakeNext does NOT return true / does NOT call MarkRunning before
// the (short) context expires. With the buggy code, wakeNext would return
// true immediately on the very first wake.
func TestSubagent_WakeNextReParksOnStaleInboxSignal(t *testing.T) {
	t.Parallel()

	workerP := &queueProvider{id: "test/worker"}
	worker := agent.New("worker", "Worker.", agent.WithModel(workerP))
	root := agent.New("root", "Root.", agent.WithModel(workerP), agent.WithToolSets(subagent.NewToolSet()))
	agent.WithSubAgents(worker)(root)

	tm := team.New(team.WithAgents(root, worker))
	rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	// Build a real session ending on an assistant message (the dangerous tail
	// shape that caused the original bug to surface as "unsupported assistant
	// prefill" provider errors).
	sess := session.New(
		session.WithUserMessage("hi"),
		session.WithAgentName("worker"),
	)
	sess.AddMessage(session.NewAgentMessage("worker", &chat.Message{
		Role:    chat.MessageRoleAssistant,
		Content: "prior assistant turn",
	}))

	// Register a Handle in the manager via a fake runner so we get a real
	// Handle without driving an actual engine loop.
	parent := session.New(session.WithUserMessage("root"))
	cfg := subagent.StartConfig{
		Parent:    parent,
		AgentName: "worker",
		Title:     "unit",
	}
	// Use a private runner that just keeps the loop alive until ctx done.
	mgr := subagent.NewManager(blockingFakeRunner{})
	h, err := mgr.StartChild(t.Context(), cfg, sess)
	require.NoError(t, err)
	defer func() { _ = mgr.Stop(h.ID()) }()

	// Construct a childWakePolicy directly with a controllable, pre-fired
	// child-inbox signal channel. The corresponding manager inbox for the
	// fake parent has no envelopes, so DrainParentInbox returns nil.
	staleChildSig := make(chan struct{}, 1)
	staleChildSig <- struct{}{}
	staleDirectSig := make(chan struct{}, 1)

	policy := &childWakePolicy{
		runner:         newRootSessionRunner(rt),
		h:              h,
		childInboxSig:  staleChildSig,
		directInboxSig: staleDirectSig,
		steerInboxSig:  h.SteerInboxSignal(),
	}

	// Run wakeNext in a goroutine with a short context. Without the fix,
	// wakeNext returns true immediately after consuming the stale tick;
	// with the fix, it re-parks and waits for ctx to expire, returning
	// false.
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	info := turnInfo{sess: sess, agent: worker}
	events := make(chan Event, 16)
	done := make(chan bool, 1)
	go func() {
		done <- policy.wakeNext(ctx, nil, info, events)
	}()

	select {
	case ret := <-done:
		assert.False(t, ret,
			"wakeNext must re-park on stale signal with empty drain and return false on ctx timeout, not run another turn")
	case <-time.After(2 * time.Second):
		t.Fatal("wakeNext did not return within 2s — re-parking loop may be stuck")
	}

	// Status must NOT be Running: the bug would call MarkRunning before
	// returning true. The fix only marks running when real work is injected.
	assert.NotEqual(t, subagent.StatusRunning, h.Status(),
		"wakeNext must not transition the handle to Running on a stale signal")
}

// TestSubagent_SteerModeRoutesToSteerInbox verifies that a steer-mode
// message is routed to the steer inbox and delivered mid-turn (via drainMidTurn),
// while a followup-mode message goes to the regular inbox and is NOT drained by
// drainMidTurn. All content is injected as plain user messages (no wrapping).
func TestSubagent_SteerModeRoutesToSteerInbox(t *testing.T) {
	t.Parallel()

	workerP := &queueProvider{id: "test/worker"}
	worker := agent.New("worker", "Worker.", agent.WithModel(workerP))
	root := agent.New("root", "Root.", agent.WithModel(workerP), agent.WithToolSets(subagent.NewToolSet()))
	agent.WithSubAgents(worker)(root)

	tm := team.New(team.WithAgents(root, worker))
	rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	// Build a session with a user + assistant pair so it's valid for
	// both between-turn and mid-turn injection.
	sess := session.New(
		session.WithUserMessage("hi"),
		session.WithAgentName("worker"),
	)
	sess.AddMessage(session.NewAgentMessage("worker", &chat.Message{
		Role:    chat.MessageRoleAssistant,
		Content: "previous turn",
	}))

	parent := session.New(session.WithUserMessage("root"))
	cfg := subagent.StartConfig{
		Parent:    parent,
		AgentName: "worker",
		Title:     "unit",
	}
	mgr := subagent.NewManager(blockingFakeRunner{})
	h, err := mgr.StartChild(t.Context(), cfg, sess)
	require.NoError(t, err)
	defer func() { _ = mgr.Stop(h.ID()) }()

	// ── Test 1: drainMidTurn with steer mode
	require.NoError(t, mgr.Send(h.ID(), subagent.Message{Content: "course correct", Mode: subagent.MessageModeSteer}))

	policy := &childWakePolicy{
		runner:         newRootSessionRunner(rt),
		h:              h,
		childInboxSig:  mgr.ParentInboxSignal(sess.ID),
		directInboxSig: h.InboxSignal(),
		steerInboxSig:  h.SteerInboxSignal(),
	}

	events := make(chan Event, 32)
	gotWork := policy.drainMidTurn(sess, events)
	require.True(t, gotWork, "drainMidTurn should return true when steer-mode message is pending")

	// The last user message in the session should be plain content (no wrapper).
	msgs := sess.GetAllMessages()
	require.NotEmpty(t, msgs)
	last := msgs[len(msgs)-1]
	assert.Equal(t, chat.MessageRoleUser, last.Message.Role)
	assert.Equal(t, "course correct", last.Message.Content)

	// ── Test 2: drainMidTurn with followup mode → plain, no wrapper.
	require.NoError(t, mgr.Send(h.ID(), subagent.Message{Content: "plain followup", Mode: subagent.MessageModeFollowUp}))
	// Followup-mode messages go to the regular inbox, NOT the steer inbox.
	// drainMidTurn should NOT see them.
	gotWork = policy.drainMidTurn(sess, events)
	assert.False(t, gotWork, "followup-mode message must not be drained by drainMidTurn")

	// Verify the followup message is still sitting in the regular inbox.
	pending := h.DrainInbox()
	require.Len(t, pending, 1)
	assert.Equal(t, "plain followup", pending[0].Content)

	// ── Test 3: drainMidTurn with empty inbox → false.
	gotWork = policy.drainMidTurn(sess, events)
	assert.False(t, gotWork)
}

// TestSubagent_SteerBetweenTurnsViaWakeNext verifies that a steer-mode
// message arriving while the child is parked (between turns) is delivered
// as plain user content through the wakeNext between-turn drain.
func TestSubagent_SteerBetweenTurnsViaWakeNext(t *testing.T) {
	t.Parallel()

	workerP := &queueProvider{id: "test/worker"}
	worker := agent.New("worker", "Worker.", agent.WithModel(workerP))
	root := agent.New("root", "Root.", agent.WithModel(workerP), agent.WithToolSets(subagent.NewToolSet()))
	agent.WithSubAgents(worker)(root)

	tm := team.New(team.WithAgents(root, worker))
	rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	sess := session.New(
		session.WithUserMessage("hi"),
		session.WithAgentName("worker"),
	)
	sess.AddMessage(session.NewAgentMessage("worker", &chat.Message{
		Role:    chat.MessageRoleAssistant,
		Content: "previous turn",
	}))

	parent := session.New(session.WithUserMessage("root"))
	cfg := subagent.StartConfig{
		Parent:    parent,
		AgentName: "worker",
		Title:     "unit",
	}
	mgr := subagent.NewManager(blockingFakeRunner{})
	h, err := mgr.StartChild(t.Context(), cfg, sess)
	require.NoError(t, err)
	defer func() { _ = mgr.Stop(h.ID()) }()

	// Pre-load a steer message into the inbox.
	require.NoError(t, mgr.Send(h.ID(), subagent.Message{Content: "between-turn steer", Mode: subagent.MessageModeSteer}))

	policy := &childWakePolicy{
		runner:         newRootSessionRunner(rt),
		h:              h,
		childInboxSig:  mgr.ParentInboxSignal(sess.ID),
		directInboxSig: h.InboxSignal(),
		steerInboxSig:  h.SteerInboxSignal(),
	}

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	info := turnInfo{sess: sess, agent: worker}
	events := make(chan Event, 32)
	ret := policy.wakeNext(ctx, nil, info, events)
	require.True(t, ret, "wakeNext must return true when a steer-mode message is available")

	msgs := sess.GetAllMessages()
	require.NotEmpty(t, msgs)
	last := msgs[len(msgs)-1]
	assert.Equal(t, chat.MessageRoleUser, last.Message.Role)
	assert.Equal(t, "between-turn steer", last.Message.Content)
}

// blockingFakeRunner is a no-op subagent.Runner used by direct unit tests of
// wake-policy code paths. The returned done channel closes when the supplied
// context is canceled, mirroring the real Runner contract so tests don't leak
// the manager's forwarder goroutine.
type blockingFakeRunner struct{}

func (blockingFakeRunner) StartChildLoop(ctx context.Context, _ *subagent.Handle) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		<-ctx.Done()
		close(done)
	}()
	return done
}

// TestSubagent_WakeNextCloseChPrioritizedOverWork verifies that when both
// CloseCh and a real childInboxSig are ready simultaneously, wakeNext returns
// false and the handle does NOT transition to Running. Without the fix, a work
// signal that races a close request could cause the child to start another turn
// even though the parent has already asked it to shut down.
func TestSubagent_WakeNextCloseChPrioritizedOverWork(t *testing.T) {
	t.Parallel()

	workerP := &queueProvider{id: "test/worker"}
	worker := agent.New("worker", "Worker.", agent.WithModel(workerP))
	root := agent.New("root", "Root.", agent.WithModel(workerP), agent.WithToolSets(subagent.NewToolSet()))
	agent.WithSubAgents(worker)(root)

	tm := team.New(team.WithAgents(root, worker))
	rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	sess := session.New(
		session.WithUserMessage("hi"),
		session.WithAgentName("worker"),
	)
	sess.AddMessage(session.NewAgentMessage("worker", &chat.Message{
		Role:    chat.MessageRoleAssistant,
		Content: "prior turn",
	}))

	parent := session.New(session.WithUserMessage("root"))
	cfg := subagent.StartConfig{
		Parent:    parent,
		AgentName: "worker",
		Title:     "unit",
	}

	mgr := subagent.NewManager(blockingFakeRunner{})
	h, err := mgr.StartChild(t.Context(), cfg, sess)
	require.NoError(t, err)
	defer func() { _ = mgr.Stop(h.ID()) }()

	// Publish a real envelope into the child's grandchild inbox AND close
	// the handle simultaneously. Both channels are ready before wakeNext
	// enters the select.
	mgr.PublishEnvelope(subagent.Envelope{
		SubAgentID:      "fake-grandchild",
		ParentSessionID: sess.ID,
		AgentName:       "leaf",
		Kind:            subagent.UpdateKindTurnCompleted,
		Status:          subagent.StatusWaiting,
		Preview:         "gc done",
	})

	// Request close so CloseCh is ready.
	_ = mgr.Close(h.ID())

	childInboxSig := mgr.ParentInboxSignal(sess.ID)
	directInboxSig := h.InboxSignal()

	policy := &childWakePolicy{
		runner:         newRootSessionRunner(rt),
		h:              h,
		childInboxSig:  childInboxSig,
		directInboxSig: directInboxSig,
		steerInboxSig:  h.SteerInboxSignal(),
	}

	info := turnInfo{sess: sess, agent: worker}
	events := make(chan Event, 32)

	ret := policy.wakeNext(t.Context(), nil, info, events)

	assert.False(t, ret,
		"wakeNext must return false when CloseCh is ready, even if real work is also available")
	assert.NotEqual(t, subagent.StatusRunning, h.Status(),
		"handle must not transition to Running when close is pending")
}

// TestSubagent_StartChildStopRace is a regression test for the data race
// where Handle.stopFn was a plain func() set by setStop AFTER the handle
// was already published into m.byID and child-registered listeners had
// run. A listener that immediately called Manager.Stop on the new handle
// would race with the setStop write, and on the losing schedule the
// cancel would silently not happen.
//
// The fix passes the cancel func into newHandle at construction time
// (atomic.Pointer[func()]), so by the time any listener or external
// caller can observe the handle, stopFn is already set.
//
// We can't reach the new childRegisteredListeners (private), so we
// exercise the equivalent path: register a listener via the public
// AddChildRegisteredListener that calls Manager.Stop on the newly-created
// handle from inside the listener. With the fix, the cancel runs and the
// child loop terminates promptly. Without the fix, the listener's Stop
// would either no-op or hit a half-published stopFn and the child loop
// would survive past the listener.
func TestSubagent_StartChildStopRace(t *testing.T) {
	t.Parallel()

	workerP := &blockingProvider{id: "test/worker-race"}
	worker := agent.New("worker", "blocking", agent.WithModel(workerP))
	root := agent.New("root", "root", agent.WithModel(workerP), agent.WithToolSets(subagent.NewToolSet()))
	agent.WithSubAgents(worker)(root)

	tm := team.New(team.WithAgents(root, worker))
	rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	// Register a listener that immediately stops the new handle. With the
	// pre-fix code, this listener fires before setStop and the cancel is
	// dropped or races; with the fix, stopFn is already wired so Stop
	// reliably cancels the child context.
	rt.subagents.AddChildRegisteredListener(func(h *subagent.Handle) {
		// Run the stop in a goroutine to avoid serializing under the
		// manager lock (StartChild holds m.mu when invoking listeners
		// in some configurations); this also better simulates the
		// concurrent-caller scenario the race fix targets.
		go func() { _ = rt.subagents.Stop(h.ID()) }()
	})

	parent := session.New(session.WithUserMessage("root"), session.WithToolsApproved(true))

	for i := range 8 {
		t.Run("iter", func(t *testing.T) {
			_ = i
			childSess := rt.newSubagentChildSession(parent, subagent.StartConfig{
				Parent:        parent,
				AgentName:     "worker",
				Task:          "block",
				ToolsApproved: true,
				Title:         "child",
			}, worker)

			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()

			h, err := rt.subagents.StartChild(ctx, subagent.StartConfig{
				Parent:        parent,
				AgentName:     "worker",
				Task:          "block",
				ToolsApproved: true,
				Title:         "child",
			}, childSess)
			require.NoError(t, err)

			// The listener stop must reliably terminate the child loop
			// even though the worker provider would otherwise block
			// indefinitely. The stop racing with StartChild's own
			// internal wiring is the exact regression we guard against.
			require.Eventually(t, func() bool {
				snap, err := rt.subagents.Get(h.ID())
				return err == nil && snap.Status.IsTerminal()
			}, 2*time.Second, 10*time.Millisecond,
				"listener-triggered Stop must cancel the child even when racing StartChild's own wiring")
		})
	}
}
