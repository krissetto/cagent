package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin"
)

// subagentTestSetup wires up a minimal parent-with-subagent topology and
// returns the pieces each test needs.
type subagentTestSetup struct {
	rt      *LocalRuntime
	parent  *agent.Agent
	worker  *agent.Agent
	parentP *queueProvider
	workerP *queueProvider
}

func newSubagentTestSetup(t *testing.T) *subagentTestSetup {
	t.Helper()

	parentP := &queueProvider{id: "test/parent"}
	workerP := &queueProvider{id: "test/worker"}

	worker := agent.New("worker", "You are the worker agent.", agent.WithModel(workerP))
	parent := agent.New("root", "You are the root agent.",
		agent.WithModel(parentP),
		agent.WithToolSets(subagent.NewToolSet()),
	)
	agent.WithSubAgents(worker)(parent)

	tm := team.New(team.WithAgents(parent, worker))

	rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	return &subagentTestSetup{
		rt: rt, parent: parent, worker: worker, parentP: parentP, workerP: workerP,
	}
}

func (s *subagentTestSetup) queueParentStream(t *testing.T, sb *streamBuilder) {
	t.Helper()
	s.parentP.mu.Lock()
	defer s.parentP.mu.Unlock()
	s.parentP.streams = append(s.parentP.streams, sb.Build())
}

func (s *subagentTestSetup) queueWorkerStream(t *testing.T, sb *streamBuilder) {
	t.Helper()
	s.workerP.mu.Lock()
	defer s.workerP.mu.Unlock()
	s.workerP.streams = append(s.workerP.streams, sb.Build())
}

// TestSubagent_ParentWakesOnChildTurn verifies the headline runtime
// behavior: a parent that has finished its own turn remains alive while
// a subagent is in flight, receives the subagent's update automatically,
// and takes another turn of its own in response.
// TestSubagent_ChildLoopDrainsGrandchildEnvelopeBeforeNextTurn is a
// regression test for a nested-subagent bug where a middle child, idle
// between turns, was woken by a grandchild's envelope and immediately
// tried to start another turn with its session history still ending on
// an assistant message. Anthropic models without assistant-message
// prefill support reject that conversation shape with HTTP 400
// ("conversation must end with a user message").
//
// This test reproduces the exact wake-up path without a real model:
//   - start a middle child (worker) whose first turn ends with a plain
//     assistant message, so the session tail is role=assistant;
//   - while the worker is idle-waiting, publish a grandchild-style
//     envelope onto the worker's own parent inbox (the worker is the
//     "parent" from the grandchild's perspective);
//   - assert that the worker's session tail flips to role=user BEFORE
//     the next turn runs.
//
// We never need a second provider stream: the test asserts on session
// state, not on a subsequent model call. The point is that the history
// is now valid for any provider by the time the next turn is about to
// send it.
func TestSubagent_ChildLoopDrainsGrandchildEnvelopeBeforeNextTurn(t *testing.T) {
	setup := newSubagentTestSetup(t)
	parent := session.New(session.WithUserMessage("root"), session.WithToolsApproved(true))

	// Worker's provider queue:
	//   1. first turn produces plain assistant content so the session tail is
	//      role=assistant when the worker goes idle;
	//   2. the next wake-up turn returns an empty stream, so no new assistant
	//      message is appended and we can assert directly on the injected
	//      user-role reminder that would be sent to Anthropic.
	setup.workerP.streams = []chat.MessageStream{
		newStreamBuilder().AddContent("first turn done").AddStopWithUsage(5, 2).Build(),
		&mockStream{},
	}

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
	defer func() { _ = setup.rt.subagents.Stop(h.ID()) }()

	// Wait for the worker's first turn to complete so it is now parked in
	// the outer select. The turn-completed envelope lands on parent's
	// inbox; we drain and discard it because we only care about the worker
	// session, not the parent.
	require.True(t, setup.rt.subagents.WaitParentInbox(ctx, parent.ID))
	_ = setup.rt.subagents.DrainParentInbox(parent.ID)

	// Let the outer select actually start parking on signals before we
	// publish the grandchild envelope. A tiny, bounded sleep is enough; we
	// don't need to simulate a real grandchild loop.
	require.Eventually(t, func() bool {
		snap, err := setup.rt.subagents.Get(h.ID())
		return err == nil && snap.Status == subagent.StatusWaiting
	}, 2*time.Second, 20*time.Millisecond)

	// Precondition: the worker's session currently ends on an assistant
	// message. This is exactly the shape Anthropic rejects as prefill.
	before := childSess.GetAllMessages()
	require.NotEmpty(t, before)
	require.Equal(t, chat.MessageRoleAssistant, before[len(before)-1].Message.Role,
		"precondition: child session must end with an assistant message before the envelope wake-up")

	// Simulate a grandchild publishing a turn-completed envelope onto the
	// worker's parent inbox. The StartChildLoop outer select selects on
	// this inbox via ParentInboxSignal(workerID), so publishing here wakes
	// the worker loop the same way a real grandchild would.
	setup.rt.subagents.PublishEnvelope(subagent.Envelope{
		SubAgentID:      "grandchild-for-test",
		ParentSessionID: h.ID(),
		AgentName:       "grandchild",
		Kind:            subagent.UpdateKindTurnCompleted,
		Status:          subagent.StatusWaiting,
		Preview:         "grandchild preview",
		At:              time.Now(),
	})

	// The worker loop should drain the envelope into its own session as a
	// user-role reminder before starting another turn. Assert on the
	// session tail directly, which is the thing the model would actually
	// receive on the next RunStream call.
	require.Eventually(t, func() bool {
		msgs := childSess.GetAllMessages()
		if len(msgs) == 0 {
			return false
		}
		last := msgs[len(msgs)-1]
		return last.Message.Role == chat.MessageRoleUser &&
			strings.Contains(last.Message.Content, "<subagent_update>")
	}, 3*time.Second, 20*time.Millisecond,
		"worker loop must drain grandchild envelope into its session as a user-role reminder before the next turn")
}

func TestSubagent_ParentWakesOnChildTurn(t *testing.T) {
	setup := newSubagentTestSetup(t)

	// Parent's first turn: call subagent_start.
	parentT1 := newStreamBuilder().
		AddToolCallName("tc1", subagent.ToolNameStart).
		AddToolCallArguments("tc1", `{"agent":"worker","task":"do the thing"}`).
		AddStopWithUsage(10, 5)

	// Parent's second turn: replies after receiving the subagent envelope.
	parentT2 := newStreamBuilder().
		AddContent("Thanks, subagent is done.").
		AddStopWithUsage(12, 6)

	// Worker's only turn: produces a final answer.
	// Because title generation now runs concurrently with the first child turn
	// (both against the worker provider's FIFO stream queue), either goroutine
	// can pop the next stream. We queue two identical streams so that whichever
	// path wins the race, the completed-turn envelope still carries the
	// expected preview and the other stream absorbs the title-gen attempt.
	workerT1a := newStreamBuilder().
		AddContent("Subagent result: 42").
		AddStopWithUsage(8, 4)
	workerT1b := newStreamBuilder().
		AddContent("Subagent result: 42").
		AddStopWithUsage(8, 4)

	setup.queueParentStream(t, parentT1)
	setup.queueParentStream(t, parentT2)
	setup.queueWorkerStream(t, workerT1a)
	setup.queueWorkerStream(t, workerT1b)

	sess := session.New(session.WithUserMessage("Please delegate."), session.WithToolsApproved(true))
	sess.Title = "subagent wakeup"

	ctx, cancel := timeoutCtx(t, 5*time.Second)
	defer cancel()

	var (
		sawStarted, sawUpdate bool
		parentResponseText    string
		stopCount             int
	)
	events := setup.rt.RunStream(ctx, sess)
	for ev := range events {
		switch e := ev.(type) {
		case *SubAgentStartedEvent:
			sawStarted = true
		case *SubAgentUpdateEvent:
			sawUpdate = true
			require.Equal(t, subagent.UpdateKindTurnCompleted, e.Envelope.Kind)
			require.Contains(t, e.Envelope.Preview, "Subagent result: 42")
		case *AgentChoiceEvent:
			parentResponseText += e.Content
		case *StreamStoppedEvent:
			stopCount++
		}
	}

	assert.True(t, sawStarted, "expected subagent_started event")
	assert.True(t, sawUpdate, "expected subagent_update event")
	assert.Contains(t, parentResponseText, "Thanks, subagent is done.",
		"parent must take a second turn after child turn-completed envelope")
	assert.GreaterOrEqual(t, stopCount, 1)
}

// TestSubagent_ParentSendAndChildSecondTurn verifies parent→child
// messaging and that the runtime-managed child loop can execute a
// second turn after receiving a new message from its parent. This test
// targets the child-loop/runtime integration directly rather than the
// root event stream.
func TestSubagent_ParentSendAndChildSecondTurn(t *testing.T) {
	setup := newSubagentTestSetup(t)
	parent := session.New(session.WithUserMessage("root"), session.WithToolsApproved(true))

	workerTurn1 := newStreamBuilder().AddContent("first").AddStopWithUsage(5, 2)
	workerTurn2 := newStreamBuilder().AddContent("second").AddStopWithUsage(5, 2)
	setup.queueWorkerStream(t, workerTurn1)
	setup.queueWorkerStream(t, workerTurn2)

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

	// First child turn.
	require.True(t, setup.rt.subagents.WaitParentInbox(ctx, parent.ID))
	envs := setup.rt.subagents.DrainParentInbox(parent.ID)
	require.Len(t, envs, 1)
	assert.Contains(t, envs[0].Preview, "first")

	// Send another message to trigger a second turn.
	require.NoError(t, setup.rt.subagents.Send(h.ID(), subagent.Message{Content: "continue"}))
	require.True(t, setup.rt.subagents.WaitParentInbox(ctx, parent.ID))
	envs = setup.rt.subagents.DrainParentInbox(parent.ID)
	require.Len(t, envs, 1)
	assert.Contains(t, envs[0].Preview, "second")
}

func TestBuildSubagentInitialUserMessage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  subagent.StartConfig
		want string
	}{
		{
			name: "task only",
			cfg:  subagent.StartConfig{Task: "Review the latest failures"},
			want: "Review the latest failures",
		},
		{
			name: "initial message overrides synthesized task text",
			cfg: subagent.StartConfig{
				Task:           "ignored",
				InitialMessage: subagent.Message{Content: "Use this exact message"},
			},
			want: "Use this exact message",
		},
		{
			name: "empty config falls back to placeholder",
			cfg:  subagent.StartConfig{},
			want: "Please proceed.",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, buildSubagentInitialUserMessage(tc.cfg))
		})
	}
}

// TestSubagent_CloseEndsChildCleanly verifies that Close causes the
// child loop to emit a closed envelope and terminate.
func TestSubagent_CloseEndsChildCleanly(t *testing.T) {
	setup := newSubagentTestSetup(t)
	parent := session.New(session.WithUserMessage("root"), session.WithToolsApproved(true))
	workerTurn1 := newStreamBuilder().AddContent("hello").AddStopWithUsage(5, 2)
	setup.queueWorkerStream(t, workerTurn1)

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

	// First turn completes.
	require.True(t, setup.rt.subagents.WaitParentInbox(ctx, parent.ID))
	envs := setup.rt.subagents.DrainParentInbox(parent.ID)
	require.Len(t, envs, 1)
	require.Equal(t, subagent.UpdateKindTurnCompleted, envs[0].Kind)

	// Then close the child and expect a terminal envelope.
	require.NoError(t, setup.rt.subagents.Close(h.ID()))
	require.True(t, setup.rt.subagents.WaitParentInbox(ctx, parent.ID))
	envs = setup.rt.subagents.DrainParentInbox(parent.ID)
	require.Len(t, envs, 1)
	require.Equal(t, subagent.UpdateKindClosed, envs[0].Kind)
}

// TestSubagent_InjectsImplicitUserMessage verifies that envelope
// delivery adds a user-role reminder to the parent session so the model
// can observe the child's output on its next turn.
func TestSubagent_InjectsImplicitUserMessage(t *testing.T) {
	setup := newSubagentTestSetup(t)

	parentTurn1 := newStreamBuilder().
		AddToolCallName("tc1", subagent.ToolNameStart).
		AddToolCallArguments("tc1", `{"agent":"worker","task":"work"}`).
		AddStopWithUsage(10, 5)
	parentTurn2 := newStreamBuilder().AddContent("noted").AddStopWithUsage(5, 2)
	workerTurn1 := newStreamBuilder().AddContent("worker result").AddStopWithUsage(5, 2)

	setup.queueParentStream(t, parentTurn1)
	setup.queueParentStream(t, parentTurn2)
	setup.queueWorkerStream(t, workerTurn1)

	sess := session.New(session.WithUserMessage("Delegate"), session.WithToolsApproved(true))
	ctx, cancel := timeoutCtx(t, 5*time.Second)
	defer cancel()

	events := setup.rt.RunStream(ctx, sess)
	for range events {
	}

	// Look for an implicit user message carrying the envelope tag.
	var foundImplicit bool
	for _, item := range sess.Messages {
		if !item.IsMessage() {
			continue
		}
		m := item.Message.Message
		if m.Role != chat.MessageRoleUser {
			continue
		}
		if strings.Contains(m.Content, "<subagent_update>") && item.Message.Implicit {
			foundImplicit = true
			break
		}
	}
	assert.True(t, foundImplicit, "expected implicit user envelope message in parent history")
}

// TestSubagent_SafeTimeDelivery verifies that child updates are not
// injected into the parent while the parent is still mid-stream; the
// envelope must appear only after the parent reaches the safe point that
// follows tool execution.
func TestSubagent_SafeTimeDelivery(t *testing.T) {
	setup := newSubagentTestSetup(t)

	parentTurn1 := newStreamBuilder().
		AddContent("Working...").
		AddToolCallName("tc1", subagent.ToolNameStart).
		AddToolCallArguments("tc1", `{"agent":"worker","task":"work"}`).
		AddStopWithUsage(10, 5)
	parentTurn2 := newStreamBuilder().AddContent("Observed child update").AddStopWithUsage(5, 2)
	workerTurn1 := newStreamBuilder().AddContent("child result").AddStopWithUsage(5, 2)

	setup.queueParentStream(t, parentTurn1)
	setup.queueParentStream(t, parentTurn2)
	setup.queueWorkerStream(t, workerTurn1)

	sess := session.New(session.WithUserMessage("Delegate"), session.WithToolsApproved(true))
	ctx, cancel := timeoutCtx(t, 5*time.Second)
	defer cancel()

	var ordered []string
	for ev := range setup.rt.RunStream(ctx, sess) {
		switch e := ev.(type) {
		case *AgentChoiceEvent:
			ordered = append(ordered, "agent_choice:"+e.Content)
		case *SubAgentUpdateEvent:
			ordered = append(ordered, "subagent_update:"+e.Envelope.Preview)
		}
	}

	choiceIdx := -1
	updateIdx := -1
	for i, item := range ordered {
		if strings.HasPrefix(item, "agent_choice:Working") && choiceIdx == -1 {
			choiceIdx = i
		}
		if strings.HasPrefix(item, "subagent_update:") && updateIdx == -1 {
			updateIdx = i
		}
	}
	require.NotEqual(t, -1, choiceIdx)
	require.NotEqual(t, -1, updateIdx)
	assert.Greater(t, updateIdx, choiceIdx, "subagent update must arrive only after parent safe point")
}

// TestSubagent_ManagerIsolatesByParent verifies that two parent sessions
// don't see each other's subagents.
func TestSubagent_ManagerIsolatesByParent(t *testing.T) {
	setup := newSubagentTestSetup(t)

	parent1 := session.New()
	parent2 := session.New()

	mgr := setup.rt.subagents
	child1 := session.New(session.WithAgentName("worker"))
	child2 := session.New(session.WithAgentName("worker"))

	ctx, cancel := timeoutCtx(t, 2*time.Second)
	defer cancel()

	h1, err := mgr.StartChild(ctx, subagent.StartConfig{
		Parent:    parent1,
		AgentName: "worker",
		Task:      "a",
	}, child1)
	require.NoError(t, err)

	_, err = mgr.StartChild(ctx, subagent.StartConfig{
		Parent:    parent2,
		AgentName: "worker",
		Task:      "b",
	}, child2)
	require.NoError(t, err)

	snaps1 := mgr.ListParent(parent1.ID)
	snaps2 := mgr.ListParent(parent2.ID)
	require.Len(t, snaps1, 1)
	require.Len(t, snaps2, 1)
	require.NotEqual(t, snaps1[0].ID, snaps2[0].ID)
	require.Equal(t, h1.ID(), snaps1[0].ID)
}

// recursiveSetup is like subagentTestSetup but with a third agent so
// worker can also spawn its own subagents.
type recursiveSetup struct {
	rt      *LocalRuntime
	root    *agent.Agent
	worker  *agent.Agent
	leaf    *agent.Agent
	rootP   *queueProvider
	workerP *queueProvider
	leafP   *queueProvider
}

type blockingStream struct {
	done <-chan struct{}
	err  func() error
}

func (s *blockingStream) Recv() (chat.MessageStreamResponse, error) {
	<-s.done
	return chat.MessageStreamResponse{}, s.err()
}

func (s *blockingStream) Close() {}

type blockingProvider struct{ id string }

func (p *blockingProvider) ID() string { return p.id }
func (p *blockingProvider) CreateChatCompletionStream(ctx context.Context, _ []chat.Message, _ []tools.Tool) (chat.MessageStream, error) {
	return &blockingStream{done: ctx.Done(), err: ctx.Err}, nil
}
func (p *blockingProvider) BaseConfig() base.Config { return base.Config{} }
func (p *blockingProvider) MaxTokens() int          { return 0 }

type interruptResumeProvider struct {
	id      string
	mu      sync.Mutex
	streams []func(context.Context) chat.MessageStream
}

func (p *interruptResumeProvider) ID() string { return p.id }
func (p *interruptResumeProvider) CreateChatCompletionStream(ctx context.Context, _ []chat.Message, _ []tools.Tool) (chat.MessageStream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.streams) == 0 {
		return &mockStream{}, nil
	}
	mk := p.streams[0]
	p.streams = p.streams[1:]
	return mk(ctx), nil
}
func (p *interruptResumeProvider) BaseConfig() base.Config { return base.Config{} }
func (p *interruptResumeProvider) MaxTokens() int          { return 0 }

func newRecursiveSetup(t *testing.T, managerOpts ...subagent.ManagerOption) *recursiveSetup {
	t.Helper()
	rootP := &queueProvider{id: "test/root"}
	workerP := &queueProvider{id: "test/worker"}
	leafP := &queueProvider{id: "test/leaf"}

	leaf := agent.New("leaf", "Leaf agent.", agent.WithModel(leafP))
	worker := agent.New("worker", "Worker agent.",
		agent.WithModel(workerP),
		agent.WithToolSets(subagent.NewToolSet()),
	)
	agent.WithSubAgents(leaf)(worker)
	root := agent.New("root", "Root agent.",
		agent.WithModel(rootP),
		agent.WithToolSets(subagent.NewToolSet()),
	)
	agent.WithSubAgents(worker)(root)

	tm := team.New(team.WithAgents(root, worker, leaf))
	rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	if len(managerOpts) > 0 {
		rt.subagents = subagent.NewManager(rt, managerOpts...)
	}
	return &recursiveSetup{rt: rt, root: root, worker: worker, leaf: leaf, rootP: rootP, workerP: workerP, leafP: leafP}
}

// TestSubagentRecursion_ThreeLevels verifies full recursive propagation:
// root -> worker -> leaf. The leaf wakes the worker, which wakes the root.
func TestSubagentRecursion_ThreeLevels(t *testing.T) {
	setup := newRecursiveSetup(t)

	// root starts worker
	setup.rootP.streams = []chat.MessageStream{
		newStreamBuilder().
			AddToolCallName("tc1", subagent.ToolNameStart).
			AddToolCallArguments("tc1", `{"agent":"worker","task":"delegate deeper"}`).
			AddStopWithUsage(10, 5).
			Build(),
		newStreamBuilder().AddContent("Root observed worker update.").AddStopWithUsage(5, 2).Build(),
	}
	// worker starts leaf, then reacts after leaf update.
	// Title generation runs concurrently with the worker's first turn and
	// pulls from the same FIFO provider queue, so we duplicate the first
	// stream. Whichever goroutine wins the race, one copy drives the real
	// subagent_start call and the other harmlessly absorbs the title-gen
	// attempt.
	subStartLeafA := newStreamBuilder().
		AddToolCallName("tcw1", subagent.ToolNameStart).
		AddToolCallArguments("tcw1", `{"agent":"leaf","task":"solve it"}`).
		AddStopWithUsage(10, 5).
		Build()
	subStartLeafB := newStreamBuilder().
		AddToolCallName("tcw1b", subagent.ToolNameStart).
		AddToolCallArguments("tcw1b", `{"agent":"leaf","task":"solve it"}`).
		AddStopWithUsage(10, 5).
		Build()
	setup.workerP.streams = []chat.MessageStream{
		subStartLeafA,
		subStartLeafB,
		newStreamBuilder().AddContent("Worker observed leaf update.").AddStopWithUsage(5, 2).Build(),
	}
	// leaf answers; duplicate for the same title-gen race reason as worker.
	setup.leafP.streams = []chat.MessageStream{
		newStreamBuilder().AddContent("Leaf result.").AddStopWithUsage(5, 2).Build(),
		newStreamBuilder().AddContent("Leaf result.").AddStopWithUsage(5, 2).Build(),
	}

	sess := session.New(session.WithUserMessage("Go recursive"), session.WithToolsApproved(true))
	ctx, cancel := timeoutCtx(t, 5*time.Second)
	defer cancel()

	var rootText strings.Builder
	var sawWorkerEnvelope bool
	for ev := range setup.rt.RunStream(ctx, sess) {
		switch e := ev.(type) {
		case *SubAgentUpdateEvent:
			if e.Envelope.AgentName == "worker" {
				sawWorkerEnvelope = true
			}
		case *AgentChoiceEvent:
			rootText.WriteString(e.Content)
		}
	}

	assert.True(t, sawWorkerEnvelope, "worker should publish up to root")
	assert.Contains(t, rootText.String(), "Root observed worker update.")

	// Verify the leaf actually ran and produced output inside the worker chain.
	desc := setup.rt.subagents.Descendants(sess.ID)
	var leafID string
	for _, d := range desc {
		if d.AgentName == "leaf" {
			leafID = d.ID
		}
	}
	require.NotEmpty(t, leafID, "leaf descendant must exist")
	leafSess, err := setup.rt.subagents.Session(leafID)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return strings.Contains(leafSess.GetLastAssistantMessageContent(), "Leaf result.")
	}, 3*time.Second, 20*time.Millisecond,
		"leaf must eventually produce its assistant content")
}

// TestSubagentRecursion_MiddleChildWakesOnGrandchild verifies that a child
// waiting on its parent still wakes when one of its own descendants emits an envelope.
func TestSubagentRecursion_MiddleChildWakesOnGrandchild(t *testing.T) {
	setup := newRecursiveSetup(t)

	setup.rootP.streams = []chat.MessageStream{
		newStreamBuilder().
			AddToolCallName("tc1", subagent.ToolNameStart).
			AddToolCallArguments("tc1", `{"agent":"worker","task":"delegate deeper"}`).
			AddStopWithUsage(10, 5).
			Build(),
		newStreamBuilder().AddContent("root got it").AddStopWithUsage(5, 2).Build(),
	}
	setup.workerP.streams = []chat.MessageStream{
		newStreamBuilder().
			AddToolCallName("tcw1", subagent.ToolNameStart).
			AddToolCallArguments("tcw1", `{"agent":"leaf","task":"solve it"}`).
			AddStopWithUsage(10, 5).
			Build(),
		newStreamBuilder().
			AddToolCallName("tcw1b", subagent.ToolNameStart).
			AddToolCallArguments("tcw1b", `{"agent":"leaf","task":"solve it"}`).
			AddStopWithUsage(10, 5).
			Build(),
		newStreamBuilder().AddContent("worker woke after leaf").AddStopWithUsage(5, 2).Build(),
	}
	setup.leafP.streams = []chat.MessageStream{
		newStreamBuilder().AddContent("leaf output").AddStopWithUsage(5, 2).Build(),
		newStreamBuilder().AddContent("leaf output").AddStopWithUsage(5, 2).Build(),
	}

	sess := session.New(session.WithUserMessage("Go recursive"), session.WithToolsApproved(true))
	ctx, cancel := timeoutCtx(t, 5*time.Second)
	defer cancel()

	for range setup.rt.RunStream(ctx, sess) {
	}

	desc := setup.rt.subagents.Descendants(sess.ID)
	var workerID string
	for _, d := range desc {
		if d.AgentName == "worker" {
			workerID = d.ID
			break
		}
	}
	require.NotEmpty(t, workerID)
	workerSess, err := setup.rt.subagents.Session(workerID)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return strings.Contains(workerSess.GetLastAssistantMessageContent(), "worker woke after leaf")
	}, 3*time.Second, 20*time.Millisecond,
		"worker must eventually produce 'worker woke after leaf' content")
}

// TestSubagentRecursion_NestedFinalizeDoesNotTriggerAssistantPrefill is a
// regression test for a bug observed with providers like Anthropic: a middle
// child (worker) starts a grandchild (leaf), waits for the grandchild to
// finish, then itself finishes. Right at the handoff from "grandchild update
// drained" to "worker final assistant reply committed", a stale inbox signal
// could wake the worker loop for one extra zero-work turn. That next turn sent
// a conversation still ending on an assistant message, which Anthropic rejects
// with HTTP 400 ("The conversation must end with a user message.").
//
// This test exercises the real recursive path end-to-end and asserts that the
// root session completes without any ErrorEvent mentioning assistant prefill.
func TestSubagentRecursion_NestedFinalizeDoesNotTriggerAssistantPrefill(t *testing.T) {
	setup := newRecursiveSetup(t)

	setup.rootP.streams = []chat.MessageStream{
		newStreamBuilder().
			AddToolCallName("tc1", subagent.ToolNameStart).
			AddToolCallArguments("tc1", `{"agent":"worker","task":"delegate deeper"}`).
			AddStopWithUsage(10, 5).
			Build(),
		newStreamBuilder().
			AddContent("root saw worker finish").
			AddStopWithUsage(6, 3).
			Build(),
	}

	// Worker first starts the leaf, then after the leaf envelope arrives it
	// replies with a plain assistant message and stops. The duplicated first
	// stream absorbs the concurrent child-title-generation race.
	setup.workerP.streams = []chat.MessageStream{
		newStreamBuilder().
			AddToolCallName("tcw1", subagent.ToolNameStart).
			AddToolCallArguments("tcw1", `{"agent":"leaf","task":"solve it"}`).
			AddStopWithUsage(10, 5).
			Build(),
		newStreamBuilder().
			AddToolCallName("tcw1b", subagent.ToolNameStart).
			AddToolCallArguments("tcw1b", `{"agent":"leaf","task":"solve it"}`).
			AddStopWithUsage(10, 5).
			Build(),
		newStreamBuilder().
			AddContent("worker finalized after leaf").
			AddStopWithUsage(6, 3).
			Build(),
	}

	// Leaf emits one assistant response then exits. Duplicate it for the same
	// title-generation race reason as above.
	setup.leafP.streams = []chat.MessageStream{
		newStreamBuilder().AddContent("leaf output").AddStopWithUsage(5, 2).Build(),
		newStreamBuilder().AddContent("leaf output").AddStopWithUsage(5, 2).Build(),
	}

	sess := session.New(session.WithUserMessage("Go recursive"), session.WithToolsApproved(true))
	ctx, cancel := timeoutCtx(t, 5*time.Second)
	defer cancel()

	var errs []string
	for ev := range setup.rt.RunStream(ctx, sess) {
		if errEv, ok := ev.(*ErrorEvent); ok {
			errs = append(errs, errEv.Error)
		}
	}

	for _, errText := range errs {
		assert.NotContains(t, errText, "assistant prefill")
		assert.NotContains(t, errText, "conversation must end with a user message")
	}
	assert.NotEmpty(t, sess.GetLastAssistantMessageContent(),
		"root session should still complete normally after nested subagent finalization")
}

// TestSubagentRecursion_DepthCapEnforcedMidTree verifies that a middle child
// cannot start a grandchild once the manager depth cap is reached.
func TestSubagentRecursion_DepthCapEnforcedMidTree(t *testing.T) {
	setup := newRecursiveSetup(t, subagent.WithMaxDepth(1))

	setup.rootP.streams = []chat.MessageStream{
		newStreamBuilder().
			AddToolCallName("tc1", subagent.ToolNameStart).
			AddToolCallArguments("tc1", `{"agent":"worker","task":"delegate deeper"}`).
			AddStopWithUsage(10, 5).
			Build(),
	}
	// Duplicate the worker's first-turn stream so title generation (which runs
	// concurrently with the first turn against the same provider queue) can't
	// starve the actual subagent_start call for leaf.
	setup.workerP.streams = []chat.MessageStream{
		newStreamBuilder().
			AddToolCallName("tcw1", subagent.ToolNameStart).
			AddToolCallArguments("tcw1", `{"agent":"leaf","task":"solve it"}`).
			AddStopWithUsage(10, 5).
			Build(),
		newStreamBuilder().
			AddToolCallName("tcw1b", subagent.ToolNameStart).
			AddToolCallArguments("tcw1b", `{"agent":"leaf","task":"solve it"}`).
			AddStopWithUsage(10, 5).
			Build(),
	}

	sess := session.New(session.WithUserMessage("Go recursive"), session.WithToolsApproved(true))
	ctx, cancel := timeoutCtx(t, 5*time.Second)
	defer cancel()
	for range setup.rt.RunStream(ctx, sess) {
	}

	desc := setup.rt.subagents.Descendants(sess.ID)
	var workerID string
	for _, d := range desc {
		if d.AgentName == "worker" {
			workerID = d.ID
			break
		}
	}
	require.NotEmpty(t, workerID)
	workerSess, err := setup.rt.subagents.Session(workerID)
	require.NoError(t, err)

	var foundDepthErr bool
	for _, msg := range workerSess.GetAllMessages() {
		if msg.Message.Role != chat.MessageRoleTool {
			continue
		}
		if strings.Contains(msg.Message.Content, "depth limit exceeded") {
			foundDepthErr = true
			break
		}
	}
	assert.True(t, foundDepthErr, "worker should receive a tool error for depth overflow")
}

// TestSubagentRecursion_CascadeStop verifies that stopping a middle child also
// stops its descendants.
func TestSubagentRecursion_CascadeStop(t *testing.T) {
	setup := newRecursiveSetup(t)

	setup.workerP.streams = []chat.MessageStream{
		newStreamBuilder().
			AddToolCallName("tcw1", subagent.ToolNameStart).
			AddToolCallArguments("tcw1", `{"agent":"leaf","task":"solve it"}`).
			AddStopWithUsage(10, 5).
			Build(),
	}
	setup.leafP.streams = []chat.MessageStream{
		newStreamBuilder().AddContent("leaf output").AddStopWithUsage(5, 2).Build(),
	}

	parent := session.New(session.WithUserMessage("root"), session.WithToolsApproved(true))
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

	// Wait until a leaf descendant has been spawned by the worker loop.
	deadline := time.Now().Add(2 * time.Second)
	var leafSeen bool
	for time.Now().Before(deadline) {
		for _, d := range setup.rt.subagents.Descendants(parent.ID) {
			if d.AgentName == "leaf" {
				leafSeen = true
			}
		}
		if leafSeen {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.True(t, leafSeen, "worker should have created a leaf descendant")

	// Stop the worker subtree and wait for descendants to reach terminal state.
	require.NoError(t, setup.rt.subagents.Stop(h.ID()))
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		all := setup.rt.subagents.Descendants(parent.ID)
		allTerminal := true
		for _, d := range all {
			if !d.Status.IsTerminal() {
				allTerminal = false
				break
			}
		}
		if allTerminal {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("descendants did not reach terminal state after cascade stop")
}

func TestSubagentRecursion_ExcludedToolsPropagateToGrandchildren(t *testing.T) {
	setup := newRecursiveSetup(t)

	setup.rootP.streams = []chat.MessageStream{
		newStreamBuilder().
			AddToolCallName("tc1", subagent.ToolNameStart).
			AddToolCallArguments("tc1", `{"agent":"worker","task":"delegate deeper"}`).
			AddStopWithUsage(10, 5).
			Build(),
	}
	setup.workerP.streams = []chat.MessageStream{
		newStreamBuilder().
			AddToolCallName("tcw1", subagent.ToolNameStart).
			AddToolCallArguments("tcw1", `{"agent":"leaf","task":"solve it"}`).
			AddStopWithUsage(10, 5).
			Build(),
		newStreamBuilder().
			AddToolCallName("tcw1b", subagent.ToolNameStart).
			AddToolCallArguments("tcw1b", `{"agent":"leaf","task":"solve it"}`).
			AddStopWithUsage(10, 5).
			Build(),
	}
	setup.leafP.streams = []chat.MessageStream{
		newStreamBuilder().AddContent("leaf output").AddStopWithUsage(5, 2).Build(),
		newStreamBuilder().AddContent("leaf output").AddStopWithUsage(5, 2).Build(),
	}

	sess := session.New(
		session.WithUserMessage("Go recursive"),
		session.WithToolsApproved(true),
		session.WithExcludedTools([]string{builtin.ToolNameRunSkill}),
	)
	ctx, cancel := timeoutCtx(t, 5*time.Second)
	defer cancel()
	for range setup.rt.RunStream(ctx, sess) {
	}

	desc := setup.rt.subagents.Descendants(sess.ID)
	var leafID string
	for _, d := range desc {
		if d.AgentName == "leaf" {
			leafID = d.ID
			break
		}
	}
	require.NotEmpty(t, leafID, "leaf descendant must exist")
	leafSess, err := setup.rt.subagents.Session(leafID)
	require.NoError(t, err)
	assert.Contains(t, leafSess.ExcludedTools, builtin.ToolNameRunSkill,
		"tools excluded on the root session must remain excluded for grandchildren")
}

// TestSubagentRecursion_ContextCancelCascades verifies that cancelling the
// root context tears down the whole descendant tree.
func TestSubagentRecursion_ContextCancelCascades(t *testing.T) {
	setup := newRecursiveSetup(t)

	setup.rootP.streams = []chat.MessageStream{
		newStreamBuilder().
			AddToolCallName("tc1", subagent.ToolNameStart).
			AddToolCallArguments("tc1", `{"agent":"worker","task":"delegate deeper"}`).
			AddStopWithUsage(10, 5).
			Build(),
	}
	setup.workerP.streams = []chat.MessageStream{
		newStreamBuilder().
			AddToolCallName("tcw1", subagent.ToolNameStart).
			AddToolCallArguments("tcw1", `{"agent":"leaf","task":"solve it"}`).
			AddStopWithUsage(10, 5).
			Build(),
	}
	setup.leafP.streams = []chat.MessageStream{
		newStreamBuilder().AddContent("leaf output").AddStopWithUsage(5, 2).Build(),
	}

	sess := session.New(session.WithUserMessage("Go recursive"), session.WithToolsApproved(true))
	ctx, cancel := context.WithCancel(t.Context())
	stream := setup.rt.RunStream(ctx, sess)
	cancel()
	for range stream {
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		all := setup.rt.subagents.Descendants(sess.ID)
		allTerminal := true
		for _, d := range all {
			if !d.Status.IsTerminal() {
				allTerminal = false
				break
			}
		}
		if allTerminal {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("descendants did not reach terminal state after root context cancel")
}

// --- helpers ---

func timeoutCtx(t *testing.T, d time.Duration) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(t.Context(), d)
}

func TestSubagentChildTitleIsGeneratedByChildModel(t *testing.T) {
	t.Parallel()

	setup := newSubagentTestSetup(t)
	parent := session.New(session.WithUserMessage("root"), session.WithToolsApproved(true))

	// Title generation now starts concurrently with the child's first turn,
	// so either goroutine may consume the next queued provider stream first.
	// Keep the assertion order-independent: one output must become the child
	// session title and the other must appear in the completed-turn envelope.
	setup.queueWorkerStream(t, newStreamBuilder().AddContent("work result").AddStopWithUsage(5, 2))
	setup.queueWorkerStream(t, newStreamBuilder().AddContent("Generated child title").AddStopWithUsage(3, 1))

	childSess := setup.rt.newSubagentChildSession(parent, subagent.StartConfig{
		Parent:        parent,
		AgentName:     "worker",
		Task:          "write a summary of the latest CI failures",
		ToolsApproved: true,
		Title:         "", // child titles should be generated, not pre-seeded
	}, setup.worker)

	ctx, cancel := timeoutCtx(t, 5*time.Second)
	defer cancel()

	h, err := setup.rt.subagents.StartChild(ctx, subagent.StartConfig{
		Parent:        parent,
		AgentName:     "worker",
		Task:          "write a summary of the latest CI failures",
		ToolsApproved: true,
		Title:         "",
	}, childSess)
	require.NoError(t, err)
	defer func() { _ = setup.rt.subagents.Stop(h.ID()) }()

	// Wait for the first real child turn to complete so the session is fully live.
	require.True(t, setup.rt.subagents.WaitParentInbox(ctx, parent.ID))
	envs := setup.rt.subagents.DrainParentInbox(parent.ID)
	require.NotEmpty(t, envs)
	preview := envs[len(envs)-1].Preview
	assert.Contains(t, []string{"work result", "Generated child title"}, preview,
		"first completed child turn should surface one of the queued outputs regardless of goroutine scheduling")

	require.Eventually(t, func() bool {
		title := strings.TrimSpace(childSess.GetTitle())
		return title == "Generated child title" || title == "work result"
	}, 3*time.Second, 20*time.Millisecond,
		"subagent session title should be generated by the child model using one of the queued title-generation outputs")

	require.Eventually(t, func() bool {
		snap, err := setup.rt.subagents.Get(h.ID())
		if err != nil {
			return false
		}
		title := strings.TrimSpace(snap.Title)
		return title == "Generated child title" || title == "work result"
	}, 3*time.Second, 20*time.Millisecond,
		"live subagent snapshots should reflect the generated session title so tabs/sidebar pick it up")
}

func TestSubagentChildTitleFailureLeavesTitleEmpty(t *testing.T) {
	t.Parallel()

	setup := newSubagentTestSetup(t)
	parent := session.New(session.WithUserMessage("root"), session.WithToolsApproved(true))

	// Intentionally queue NO worker streams. Both the child's first turn and
	// the concurrent title-generation goroutine pull from the same FIFO
	// provider queue, so with an empty queue both receive mockStream{} which
	// returns EOF immediately:
	//   - the title generator sees no output tokens and reports "empty title",
	//     publishing SessionTitle("") without persisting anything; and
	//   - the first child turn completes with no assistant content.
	// We assert the title stays empty so the UI can pick sensible fallbacks
	// rather than a hardcoded generic label.

	childSess := setup.rt.newSubagentChildSession(parent, subagent.StartConfig{
		Parent:        parent,
		AgentName:     "worker",
		Task:          "write a summary of the latest CI failures",
		ToolsApproved: true,
		Title:         "",
	}, setup.worker)

	ctx, cancel := timeoutCtx(t, 5*time.Second)
	defer cancel()

	h, err := setup.rt.subagents.StartChild(ctx, subagent.StartConfig{
		Parent:        parent,
		AgentName:     "worker",
		Task:          "write a summary of the latest CI failures",
		ToolsApproved: true,
		Title:         "",
	}, childSess)
	require.NoError(t, err)
	defer func() { _ = setup.rt.subagents.Stop(h.ID()) }()

	require.True(t, setup.rt.subagents.WaitParentInbox(ctx, parent.ID))
	_ = setup.rt.subagents.DrainParentInbox(parent.ID)

	// Give the async title-generation goroutine a moment to run through its
	// failure path, then confirm the title remained empty.
	require.Eventually(t, func() bool {
		return strings.TrimSpace(childSess.GetTitle()) == ""
	}, time.Second, 20*time.Millisecond,
		"failed title generation should leave the title empty so the UI can use better fallbacks than a generic hardcoded label")

	snap, err := setup.rt.subagents.Get(h.ID())
	require.NoError(t, err)
	assert.Empty(t, strings.TrimSpace(snap.Title),
		"live subagent snapshot should also remain empty on title-generation failure")
}

func TestSubAgentInspectDefaultsToLastOnly(t *testing.T) {
	t.Parallel()

	setup := newSubagentTestSetup(t)
	parent := session.New(session.WithUserMessage("root"), session.WithToolsApproved(true))
	childSess := session.New(session.WithAgentName("worker"))
	childSess.AddMessage(session.UserMessage("first task"))
	childSess.AddMessage(session.NewAgentMessage("worker", &chat.Message{Role: chat.MessageRoleAssistant, Content: "first answer"}))
	childSess.AddMessage(session.UserMessage("follow-up"))
	childSess.AddMessage(session.NewAgentMessage("worker", &chat.Message{Role: chat.MessageRoleAssistant, Content: "latest answer"}))

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
	defer func() { _ = setup.rt.subagents.Stop(h.ID()) }()

	toolCall := tools.ToolCall{
		ID:   "inspect-1",
		Type: "function",
		Function: tools.FunctionCall{
			Name:      subagent.ToolNameInspect,
			Arguments: `{"subagent_id":"` + subagent.ShortRef(h.ID()) + `"}`,
		},
	}

	result, err := setup.rt.handleSubagentInspect(ctx, nil, parent, toolCall, nil)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.IsError)

	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(result.Output), &out))
	assert.Equal(t, subagent.InspectModeLast, out["mode"])
	assert.Equal(t, "latest answer", out["last"])
	_, hasRecent := out["recent"]
	assert.False(t, hasRecent, "default inspect mode should not include transcript history")
}

func TestSubAgentInspectRecentAndFullModes(t *testing.T) {
	t.Parallel()

	setup := newSubagentTestSetup(t)
	parent := session.New(session.WithUserMessage("root"), session.WithToolsApproved(true))
	childSess := session.New(session.WithAgentName("worker"))
	childSess.AddMessage(session.UserMessage("first task"))
	childSess.AddMessage(session.NewAgentMessage("worker", &chat.Message{Role: chat.MessageRoleAssistant, Content: "first answer"}))
	childSess.AddMessage(session.UserMessage("follow-up"))
	childSess.AddMessage(session.NewAgentMessage("worker", &chat.Message{Role: chat.MessageRoleAssistant, Content: "latest answer"}))

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
	defer func() { _ = setup.rt.subagents.Stop(h.ID()) }()

	cases := []struct {
		name     string
		mode     string
		wantLen  int
		wantMode string
	}{
		{name: "recent", mode: subagent.InspectModeRecent, wantLen: 4, wantMode: subagent.InspectModeRecent},
		{name: "full", mode: subagent.InspectModeFull, wantLen: 4, wantMode: subagent.InspectModeFull},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			toolCall := tools.ToolCall{
				ID:   "inspect-" + tc.mode,
				Type: "function",
				Function: tools.FunctionCall{
					Name:      subagent.ToolNameInspect,
					Arguments: `{"subagent_id":"` + subagent.ShortRef(h.ID()) + `","mode":"` + tc.mode + `"}`,
				},
			}

			result, err := setup.rt.handleSubagentInspect(ctx, nil, parent, toolCall, nil)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.False(t, result.IsError)

			var out struct {
				Mode   string              `json:"mode"`
				Last   string              `json:"last"`
				Recent []map[string]string `json:"recent"`
			}
			require.NoError(t, json.Unmarshal([]byte(result.Output), &out))
			assert.Equal(t, tc.wantMode, out.Mode)
			assert.Equal(t, "latest answer", out.Last)
			assert.Len(t, out.Recent, tc.wantLen)
			assert.Equal(t, "first task", out.Recent[0]["content"])
			assert.Equal(t, "latest answer", out.Recent[len(out.Recent)-1]["content"])
		})
	}
}

func TestSubAgentInspectFullModeIsBounded(t *testing.T) {
	t.Parallel()

	setup := newSubagentTestSetup(t)
	parent := session.New(session.WithUserMessage("root"), session.WithToolsApproved(true))
	childSess := session.New(session.WithAgentName("worker"))
	for range 40 {
		childSess.AddMessage(session.UserMessage(strings.Repeat("u", 2000)))
		childSess.AddMessage(session.NewAgentMessage("worker", &chat.Message{Role: chat.MessageRoleAssistant, Content: strings.Repeat("a", 2000)}))
	}

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
	defer func() { _ = setup.rt.subagents.Stop(h.ID()) }()

	toolCall := tools.ToolCall{
		ID:   "inspect-full-bounded",
		Type: "function",
		Function: tools.FunctionCall{
			Name:      subagent.ToolNameInspect,
			Arguments: `{"subagent_id":"` + subagent.ShortRef(h.ID()) + `","mode":"full"}`,
		},
	}

	result, err := setup.rt.handleSubagentInspect(ctx, nil, parent, toolCall, nil)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.IsError)
	assert.LessOrEqual(t, len(result.Output), subagent.InspectFullMaxBytes+2048,
		"bounded inspect output should stay near the configured transcript limit")

	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(result.Output), &out))
	assert.Equal(t, true, out["truncated"])
	assert.Positive(t, int(out["omitted_messages"].(float64)))
}

func TestSubAgentSendReturnsCurrentStatus(t *testing.T) {
	t.Parallel()

	setup := newSubagentTestSetup(t)
	parent := session.New(session.WithUserMessage("root"), session.WithToolsApproved(true))
	childSess := session.New(session.WithAgentName("worker"))

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
	defer func() { _ = setup.rt.subagents.Stop(h.ID()) }()

	// Put the child into a known non-running live state.
	h.PublishTurn("done for now")

	toolCall := tools.ToolCall{
		ID:   "send-current-status",
		Type: "function",
		Function: tools.FunctionCall{
			Name:      subagent.ToolNameSend,
			Arguments: `{"subagent_id":"` + subagent.ShortRef(h.ID()) + `","message":"continue"}`,
		},
	}

	result, err := setup.rt.handleSubagentSend(ctx, nil, parent, toolCall, make(chan Event, 1))
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.IsError)

	var out map[string]string
	require.NoError(t, json.Unmarshal([]byte(result.Output), &out))
	// The handle may briefly be in StatusRunning if the wakeNext goroutine
	// has already drained the new inbox entry and called MarkRunning by the
	// time we read the snapshot. Both Waiting (just-published-turn) and
	// Running (already drained the new message) are valid live states; the
	// regression we guard against is the old behavior of always reporting
	// "running" regardless of the actual snapshot.
	assert.Contains(t, []string{subagent.StatusWaiting.String(), subagent.StatusRunning.String()}, out["status"],
		"send should report a live snapshot status, not a hardcoded value")
}

func TestSubAgentInterruptCancelsCurrentTurnButKeepsSessionAlive(t *testing.T) {
	t.Parallel()

	firstTurnStarted := make(chan struct{}, 1)
	workerProv := &interruptResumeProvider{id: "test/worker-interrupt", streams: []func(context.Context) chat.MessageStream{
		func(ctx context.Context) chat.MessageStream {
			select {
			case firstTurnStarted <- struct{}{}:
			default:
			}
			return &blockingStream{done: ctx.Done(), err: ctx.Err}
		},
		func(_ context.Context) chat.MessageStream {
			return newStreamBuilder().AddContent("resumed answer").AddStopWithUsage(5, 2).Build()
		},
	}}
	worker := agent.New("worker", "interruptible worker", agent.WithModel(workerProv))
	root := agent.New("root", "root agent", agent.WithModel(workerProv), agent.WithToolSets(subagent.NewToolSet()))
	agent.WithSubAgents(worker)(root)

	tm := team.New(team.WithAgents(root, worker))
	rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	parent := session.New(session.WithUserMessage("root"), session.WithToolsApproved(true))
	childSess := rt.newSubagentChildSession(parent, subagent.StartConfig{
		Parent:        parent,
		AgentName:     "worker",
		Task:          "work forever",
		ToolsApproved: true,
		Title:         "child",
	}, worker)

	// Use a long-lived child context and per-phase deadlines below. The old
	// test reused one 5s context for every phase (startup wait, interrupt wait,
	// negative inbox assertion, resumed-turn wait). On slower CI machines that
	// let the early Eventually calls consume most of the budget, the final
	// resumed-turn wait could inherit too little time and fail spuriously even
	// though the runtime logic was correct.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	h, err := rt.subagents.StartChild(ctx, subagent.StartConfig{
		Parent:        parent,
		AgentName:     "worker",
		Task:          "work forever",
		ToolsApproved: true,
		Title:         "child",
	}, childSess)
	require.NoError(t, err)
	defer func() { _ = rt.subagents.Stop(h.ID()) }()

	// Wait until the child has actually entered the first provider call, not
	// merely transitioned to StatusRunning. MarkRunning happens before the
	// turn reaches CreateChatCompletionStream; interrupting in that tiny window
	// is valid behaviour, but for this test it means the first blocking stream
	// was never consumed and the resumed turn would incorrectly pick it up.
	select {
	case <-firstTurnStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the first child turn to enter the provider")
	}
	require.Eventually(t, func() bool {
		snap, err := rt.subagents.Get(h.ID())
		return err == nil && snap.Status == subagent.StatusRunning
	}, time.Second, 20*time.Millisecond)

	// Interrupt the current turn. The per-turn cancel can race with the
	// child's MarkRunning, so retry until the child observes waiting.
	require.Eventually(t, func() bool {
		if err := rt.InterruptSessionByID(h.ID()); err != nil {
			return false
		}
		snap, err := rt.subagents.Get(h.ID())
		return err == nil && snap.Status == subagent.StatusWaiting
	}, 3*time.Second, 50*time.Millisecond, "interrupt should drop the child back to waiting")

	// Interrupt must NOT wake the parent inbox.
	time.Sleep(250 * time.Millisecond)
	shortCtx, shortCancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer shortCancel()
	require.False(t, rt.subagents.WaitParentInbox(shortCtx, parent.ID),
		"interrupting the stream must not notify the parent")
	assert.Empty(t, rt.subagents.DrainParentInbox(parent.ID),
		"interrupt should not publish any envelope to the parent")

	// The subagent must still be alive and accept follow-up messages.
	snap, err := rt.subagents.Get(h.ID())
	require.NoError(t, err)
	assert.Equal(t, subagent.StatusWaiting, snap.Status)
	assert.True(t, h.IsLive(), "child must remain alive after interrupt")
	require.NoError(t, rt.subagents.Send(h.ID(), subagent.Message{Content: "continue"}))

	// The next real completed turn should wake the parent through the normal
	// envelope path. Use a fresh phase deadline so this assertion doesn't
	// inherit a half-spent timeout budget from the earlier setup/interrupt
	// phases.
	resumeCtx, resumeCancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer resumeCancel()
	require.True(t, rt.subagents.WaitParentInbox(resumeCtx, parent.ID),
		"a resumed child turn should notify the parent once it completes")
	envs := rt.subagents.DrainParentInbox(parent.ID)
	require.NotEmpty(t, envs)
	assert.Equal(t, subagent.UpdateKindTurnCompleted, envs[len(envs)-1].Kind)
	assert.Equal(t, subagent.StatusWaiting, envs[len(envs)-1].Status)
	assert.Contains(t, envs[len(envs)-1].Preview, "resumed answer")
}

// TestSubAgent_ToolSetSatisfiesInterfaces is a tiny sanity test so the
// toolset package stays compatible with the tool discovery plumbing.
func TestLocalRuntimeCloseStopsLiveSubagents(t *testing.T) {
	t.Parallel()

	workerProv := &blockingProvider{id: "test/worker-close"}
	worker := agent.New("worker", "blocking worker", agent.WithModel(workerProv))
	root := agent.New("root", "root agent", agent.WithModel(workerProv), agent.WithToolSets(subagent.NewToolSet()))
	agent.WithSubAgents(worker)(root)

	tm := team.New(team.WithAgents(root, worker))
	rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	parent := session.New(session.WithUserMessage("root"), session.WithToolsApproved(true))
	childSess := rt.newSubagentChildSession(parent, subagent.StartConfig{
		Parent:        parent,
		AgentName:     "worker",
		Task:          "block forever",
		ToolsApproved: true,
		Title:         "child",
	}, worker)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	h, err := rt.subagents.StartChild(ctx, subagent.StartConfig{
		Parent:        parent,
		AgentName:     "worker",
		Task:          "block forever",
		ToolsApproved: true,
		Title:         "child",
	}, childSess)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		snap, err := rt.subagents.Get(h.ID())
		return err == nil && snap.Status == subagent.StatusRunning
	}, time.Second, 20*time.Millisecond)

	require.NoError(t, rt.Close())
	require.Eventually(t, func() bool {
		snap, err := rt.subagents.Get(h.ID())
		return err == nil && snap.Status.IsTerminal()
	}, 2*time.Second, 20*time.Millisecond, "runtime close should stop live subagents")
}

func TestSubAgent_ToolSetSatisfiesInterfaces(t *testing.T) {
	ts := subagent.NewToolSet()
	_, err := ts.Tools(t.Context())
	require.NoError(t, err)
	inst, ok := tools.As[tools.Instructable](ts)
	require.True(t, ok)
	require.Contains(t, inst.Instructions(), subagent.ToolNameStart)
}
