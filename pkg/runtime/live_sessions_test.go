package runtime

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

// providerStep is one scripted CreateChatCompletionStream call of a
// stepProvider: the stream to serve, an optional started signal (closed when
// the call begins) and an optional release gate (the call blocks until it is
// closed), so tests can act while a model turn is verifiably in flight.
type providerStep struct {
	stream  chat.MessageStream
	started chan struct{}
	release <-chan struct{}
}

// stepProvider serves scripted steps in call order. Calls beyond the script
// return an empty stream.
type stepProvider struct {
	id    string
	mu    sync.Mutex
	steps []providerStep
}

func (p *stepProvider) ID() modelsdev.ID { return modelsdev.ParseIDOrZero(p.id) }

func (p *stepProvider) CreateChatCompletionStream(ctx context.Context, _ []chat.Message, _ []tools.Tool) (chat.MessageStream, error) {
	p.mu.Lock()
	var step providerStep
	if len(p.steps) > 0 {
		step = p.steps[0]
		p.steps = p.steps[1:]
	}
	p.mu.Unlock()

	if step.started != nil {
		close(step.started)
	}
	if step.release != nil {
		select {
		case <-step.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if step.stream == nil {
		return &mockStream{}, nil
	}
	return step.stream, nil
}

func (p *stepProvider) BaseConfig() base.Config { return base.Config{} }
func (p *stepProvider) MaxTokens() int          { return 0 }

// newLiveSessionsRuntime builds a runtime with a "root" agent and a "worker"
// agent whose model is the supplied provider.
func newLiveSessionsRuntime(t *testing.T, workerProvider *stepProvider, store ModelStore, workerOpts ...agent.Opt) *LocalRuntime {
	t.Helper()

	root := agent.New("root", "root agent", agent.WithModel(&mockProvider{id: "test/mock-model", stream: &mockStream{}}))
	opts := append([]agent.Opt{agent.WithModel(workerProvider)}, workerOpts...)
	worker := agent.New("worker", "worker agent", opts...)
	tm := team.New(team.WithAgents(root, worker))

	rt, err := NewLocalRuntime(t.Context(), tm,
		WithSessionCompaction(false),
		WithModelStore(store),
	)
	require.NoError(t, err)
	return rt
}

// newWorkerSession builds a sub-session pinned to the worker agent.
func newWorkerSession(id string) *session.Session {
	return session.New(
		session.WithID(id),
		session.WithParentID("root-session"),
		session.WithAgentName("worker"),
		session.WithUserMessage("subtask"),
	)
}

// drainStream consumes a RunStream channel until it closes, bounded by the
// test's deadline through t.Context.
func drainStream(t *testing.T, stream <-chan Event) {
	t.Helper()
	for {
		select {
		case _, ok := <-stream:
			if !ok {
				return
			}
		case <-time.After(10 * time.Second):
			t.Fatal("timed out draining stream")
		}
	}
}

// waitClosed fails the test unless ch is closed within the timeout.
func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestLiveSessions_ListsRootAndActiveChildren(t *testing.T) {
	t.Parallel()

	startedA := make(chan struct{})
	startedB := make(chan struct{})
	release := make(chan struct{})
	prov := &stepProvider{id: "test/mock-model", steps: []providerStep{
		{stream: newStreamBuilder().AddStopWithUsage(1, 1).Build(), started: startedA, release: release},
		{stream: newStreamBuilder().AddStopWithUsage(1, 1).Build(), started: startedB, release: release},
	}}
	rt := newLiveSessionsRuntime(t, prov, mockModelStoreWithLimit{limit: 1000})

	rootSess := session.New(session.WithID("root-session"), session.WithUserMessage("hi"))
	rootSess.SetUsage(100, 50)

	// Two concurrent runs of the SAME agent: they must both be listed,
	// never collapsed by agent name.
	childA := newWorkerSession("child-a")
	childA.SetUsage(600, 100)
	childB := newWorkerSession("child-b")
	childB.SetUsage(10, 5)

	streamA := rt.RunStream(t.Context(), childA)
	streamB := rt.RunStream(t.Context(), childB)
	waitClosed(t, startedA, "first child turn")
	waitClosed(t, startedB, "second child turn")

	rows := rt.LiveSessions(t.Context(), rootSess)
	require.Len(t, rows, 3, "current root plus both live children")

	assert.True(t, rows[0].Current)
	assert.Equal(t, "root-session", rows[0].SessionID)
	assert.Equal(t, "root", rows[0].AgentName)
	assert.Equal(t, int64(150), rows[0].UsedTokens())
	assert.Equal(t, int64(1000), rows[0].ContextLimit)

	// Child rows are stable-sorted by agent name then session ID.
	assert.Equal(t, "child-a", rows[1].SessionID)
	assert.Equal(t, "worker", rows[1].AgentName)
	assert.Equal(t, int64(700), rows[1].UsedTokens())
	assert.Equal(t, int64(1000), rows[1].ContextLimit)
	assert.False(t, rows[1].Current)

	assert.Equal(t, "child-b", rows[2].SessionID)
	assert.Equal(t, "worker", rows[2].AgentName)
	assert.Equal(t, int64(15), rows[2].UsedTokens())

	// Ordering is deterministic across calls.
	assert.Equal(t, rows, rt.LiveSessions(t.Context(), rootSess))

	close(release)
	drainStream(t, streamA)
	drainStream(t, streamB)

	rows = rt.LiveSessions(t.Context(), rootSess)
	require.Len(t, rows, 1, "finished children drop out of the view")
	assert.True(t, rows[0].Current)
}

func TestLiveSessions_UnknownContextLimit(t *testing.T) {
	t.Parallel()

	prov := &stepProvider{id: "test/mock-model"}
	rt := newLiveSessionsRuntime(t, prov, mockModelStore{})

	rootSess := session.New(session.WithID("root-session"), session.WithUserMessage("hi"))
	rootSess.SetUsage(42, 8)

	rows := rt.LiveSessions(t.Context(), rootSess)
	require.Len(t, rows, 1, "the idle current root is always listed")
	assert.True(t, rows[0].Current)
	assert.Equal(t, int64(50), rows[0].UsedTokens())
	assert.Zero(t, rows[0].ContextLimit, "unresolvable model window reports an unknown limit")
}

// TestLiveSessions_ExcludesSessionsOutsideCurrentRootTree pins the team
// scoping contract: only descendants of the current root are listed. A stale
// root stream (the session App.NewSession/ReplaceSession swapped away from
// before it fully unregistered) and its children stay out of the view, while
// direct, duplicate same-agent, and nested descendants of current all remain.
// With a nil current, every live entry is retained.
func TestLiveSessions_ExcludesSessionsOutsideCurrentRootTree(t *testing.T) {
	t.Parallel()

	rt := newLiveSessionsRuntime(t, &stepProvider{id: "test/mock-model"}, mockModelStoreWithLimit{limit: 1000})

	current := session.New(session.WithID("root-current"), session.WithUserMessage("hi"))

	// Two concurrent same-agent children of current plus a nested sub-agent
	// under the first child.
	childA := session.New(session.WithID("child-a"), session.WithParentID("root-current"), session.WithAgentName("worker"))
	childB := session.New(session.WithID("child-b"), session.WithParentID("root-current"), session.WithAgentName("worker"))
	nested := session.New(session.WithID("nested-1"), session.WithParentID("child-a"), session.WithAgentName("worker"))

	// An unrelated root still streaming, and its child.
	staleRoot := session.New(session.WithID("root-stale"), session.WithUserMessage("old"))
	staleChild := session.New(session.WithID("stale-child"), session.WithParentID("root-stale"), session.WithAgentName("worker"))

	for _, sess := range []*session.Session{childA, childB, nested, staleRoot, staleChild} {
		rt.registerLiveSession(sess)
	}

	rows := rt.LiveSessions(t.Context(), current)
	require.Len(t, rows, 4, "current plus its direct and nested descendants only")
	assert.True(t, rows[0].Current)
	assert.Equal(t, "root-current", rows[0].SessionID)
	assert.Equal(t, "child-a", rows[1].SessionID)
	assert.Equal(t, "child-b", rows[2].SessionID)
	assert.Equal(t, "nested-1", rows[3].SessionID)

	rows = rt.LiveSessions(t.Context(), nil)
	require.Len(t, rows, 5, "a nil current retains every live entry")
	for _, row := range rows {
		assert.False(t, row.Current)
	}
}

// TestLiveSessions_KeepsNestedBackgroundAfterParentFinishes is the
// regression test for cached tree-root ancestry: a nested background agent
// (root R -> transfer child C -> background B started by C) must stay in
// current R's view after its intermediate parent C finished and
// unregistered, and after R's own stream finished too. Re-walking ParentID
// links through live entries only would break B's lineage as soon as C left
// the registry.
func TestLiveSessions_KeepsNestedBackgroundAfterParentFinishes(t *testing.T) {
	t.Parallel()

	rt := newLiveSessionsRuntime(t, &stepProvider{id: "test/mock-model"}, mockModelStoreWithLimit{limit: 1000})

	current := session.New(session.WithID("root-current"), session.WithUserMessage("hi"))
	child := session.New(session.WithID("child-1"), session.WithParentID("root-current"), session.WithAgentName("worker"))
	nested := session.New(session.WithID("nested-bg"), session.WithParentID("child-1"), session.WithAgentName("worker"))

	// Registration order mirrors the real flow: each child stream starts
	// from within its parent's still-live stream.
	rootEntry := rt.registerLiveSession(current)
	childEntry := rt.registerLiveSession(child)
	rt.registerLiveSession(nested)

	rows := rt.LiveSessions(t.Context(), current)
	require.Len(t, rows, 3, "current plus both live descendants while everything runs")

	// The intermediate parent finishes, then the root turn completes; the
	// long-running background agent stays attributed to the current root.
	rt.finishLiveSession(t.Context(), childEntry)
	rt.finishLiveSession(t.Context(), rootEntry)

	rows = rt.LiveSessions(t.Context(), current)
	require.Len(t, rows, 2, "the nested background agent survives its parent's unregistration")
	assert.True(t, rows[0].Current)
	assert.Equal(t, "root-current", rows[0].SessionID)
	assert.Equal(t, "nested-bg", rows[1].SessionID)
	assert.False(t, rows[1].Current)
}

// TestLiveSessions_CachedRootFallbackForUnregisteredParents pins the
// registration fallback for entries whose parent entry is not live: the
// parent ID itself is cached as the tree root. An orphan pointing at an
// unknown parent and a malformed ParentID cycle therefore resolve to roots
// outside the current tree and are excluded from its view, while the orphan
// is still attributed to its vanished parent's tree.
func TestLiveSessions_CachedRootFallbackForUnregisteredParents(t *testing.T) {
	t.Parallel()

	rt := newLiveSessionsRuntime(t, &stepProvider{id: "test/mock-model"}, mockModelStoreWithLimit{limit: 1000})

	current := session.New(session.WithID("root-current"), session.WithUserMessage("hi"))

	orphan := session.New(session.WithID("orphan-1"), session.WithParentID("gone"), session.WithAgentName("worker"))
	// cycle-a registers before cycle-b, so its unregistered parent cycle-b
	// becomes the cached root, inherited by cycle-b in turn.
	cycleA := session.New(session.WithID("cycle-a"), session.WithParentID("cycle-b"), session.WithAgentName("worker"))
	cycleB := session.New(session.WithID("cycle-b"), session.WithParentID("cycle-a"), session.WithAgentName("worker"))

	for _, sess := range []*session.Session{orphan, cycleA, cycleB} {
		rt.registerLiveSession(sess)
	}

	rows := rt.LiveSessions(t.Context(), current)
	require.Len(t, rows, 1, "entries rooted outside the current tree are excluded")
	assert.True(t, rows[0].Current)

	// The orphan's cached root is its vanished parent: were that parent the
	// current session, the orphan would be listed under it.
	gone := session.New(session.WithID("gone"), session.WithUserMessage("old"))
	rows = rt.LiveSessions(t.Context(), gone)
	require.Len(t, rows, 2)
	assert.Equal(t, "orphan-1", rows[1].SessionID)
}

func TestCompactLiveSession_UnknownSessionErrors(t *testing.T) {
	t.Parallel()

	rt := newLiveSessionsRuntime(t, &stepProvider{id: "test/mock-model"}, mockModelStoreWithLimit{limit: 1000})

	err := rt.CompactLiveSession(t.Context(), "no-such-session", "", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not live")
}

func TestCompactLiveSession_ExecutesAtIterationBoundary(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})
	prov := &stepProvider{id: "test/mock-model", steps: []providerStep{
		// Turn 1: content without a finish reason keeps the loop running,
		// so the queued request executes at the next iteration boundary.
		{stream: newStreamBuilder().AddContent("working on it").Build(), started: started, release: release},
		// The compaction summary call.
		{stream: newStreamBuilder().AddContent("a compact summary").AddStopWithUsage(10, 5).Build()},
		// Turn 2: natural stop ends the stream.
		{stream: newStreamBuilder().AddStopWithUsage(1, 1).Build()},
	}}
	rt := newLiveSessionsRuntime(t, prov, mockModelStoreWithLimit{limit: 100_000})

	child := newWorkerSession("child-1")
	stream := rt.RunStream(t.Context(), child)
	waitClosed(t, started, "first child turn")

	requestEvents := make(chan Event, 64)
	require.NoError(t, rt.CompactLiveSession(t.Context(), "child-1", "", NewChannelSink(requestEvents)))

	// A second request while one is pending is rejected clearly.
	err := rt.CompactLiveSession(t.Context(), "child-1", "", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already pending")

	close(release)
	drainStream(t, stream)
	close(requestEvents)

	var kinds []string
	for ev := range requestEvents {
		switch e := ev.(type) {
		case *SessionCompactionEvent:
			assert.Equal(t, "child-1", e.SessionID)
			assert.Equal(t, "worker", e.AgentName)
			if e.Status == "completed" {
				kinds = append(kinds, "completed:"+e.Outcome)
			} else {
				kinds = append(kinds, e.Status)
			}
		case *SessionSummaryEvent:
			assert.Equal(t, "child-1", e.SessionID)
			kinds = append(kinds, "summary")
		case *TokenUsageEvent:
			assert.Equal(t, "child-1", e.SessionID)
			assert.Equal(t, "worker", e.AgentName)
			kinds = append(kinds, "usage")
		}
	}
	assert.Equal(t, []string{"started", "summary", "completed:applied", "usage"}, kinds)
	assert.Equal(t, "a compact summary", child.LastSummary())
}

// TestCompactLiveSession_AcceptedRequestDrainedAtTeardown pins the shutdown
// contract: a request accepted while the session's final turn is in flight
// (so no further iteration boundary occurs) still executes during stream
// teardown and emits a terminal event, instead of being silently dropped.
func TestCompactLiveSession_AcceptedRequestDrainedAtTeardown(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})
	prov := &stepProvider{id: "test/mock-model", steps: []providerStep{
		// The one and only turn: a natural stop, gated so the request can
		// be enqueued mid-turn.
		{stream: newStreamBuilder().AddContent("done").AddStopWithUsage(1, 1).Build(), started: started, release: release},
		// The compaction summary call, issued from the teardown drain.
		{stream: newStreamBuilder().AddContent("teardown summary").AddStopWithUsage(10, 5).Build()},
	}}
	rt := newLiveSessionsRuntime(t, prov, mockModelStoreWithLimit{limit: 100_000})

	child := newWorkerSession("child-1")
	stream := rt.RunStream(t.Context(), child)
	waitClosed(t, started, "final child turn")

	requestEvents := make(chan Event, 64)
	require.NoError(t, rt.CompactLiveSession(t.Context(), "child-1", "", NewChannelSink(requestEvents)))

	close(release)
	drainStream(t, stream)

	// The teardown drain executes before the stream channel closes, so the
	// terminal compaction event is already buffered on the request sink.
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev := <-requestEvents:
			if e, ok := ev.(*SessionCompactionEvent); ok && e.Status == "completed" {
				assert.Equal(t, "child-1", e.SessionID)
				assert.Equal(t, "worker", e.AgentName)
				assert.Equal(t, CompactionOutcomeApplied, e.Outcome)
				assert.Equal(t, "teardown summary", child.LastSummary())

				// The session is gone from the registry: further requests
				// are rejected instead of stranded.
				err := rt.CompactLiveSession(t.Context(), "child-1", "", nil)
				require.Error(t, err)
				assert.Contains(t, err.Error(), "not live")
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for the terminal compaction event")
		}
	}
}

// TestCompactLiveSession_DuplicateSessionIDsCompactOnlyTargetEntry is the
// regression test for the boundary drain consuming the exact
// *liveSessionEntry of its own RunStream: with two simultaneously registered
// sessions sharing one ID, a request queued to the currently targetable
// (latest) entry must not be consumed by the older stream's iteration
// boundary or teardown drain, and must mutate only the newer in-memory
// session.
func TestCompactLiveSession_DuplicateSessionIDsCompactOnlyTargetEntry(t *testing.T) {
	t.Parallel()

	startedA := make(chan struct{})
	releaseA := make(chan struct{})
	startedB := make(chan struct{})
	releaseB := make(chan struct{})
	prov := &stepProvider{id: "test/mock-model", steps: []providerStep{
		// Older stream turn 1: a tool call keeps the stream live (the loop
		// continues to execute the call) while the newer stream registers
		// under the same session ID. A tool-call turn is used rather than a
		// bare content turn so the older stream deterministically reaches a
		// second model call regardless of the bare-EOF stop rule.
		{stream: newStreamBuilder().
			AddToolCallName("call_older", "unknown_tool").
			AddToolCallArguments("call_older", "{}").
			AddToolCallStopWithUsage(1, 1).
			Build(), started: startedA, release: releaseA},
		// Newer stream turn 1, gated so it stays live throughout.
		{stream: newStreamBuilder().AddContent("newer working").Build(), started: startedB, release: releaseB},
		// Older stream turn 2: natural stop. With the request left alone this
		// is the older stream's next model call (after the tool result feeds
		// back in); stealing the request would consume this step as the
		// compaction summary instead.
		{stream: newStreamBuilder().AddStopWithUsage(1, 1).Build()},
		// The compaction summary call, drained by the newer stream's own
		// iteration boundary.
		{stream: newStreamBuilder().AddContent("latest summary").AddStopWithUsage(10, 5).Build()},
		// Newer stream turn 2: natural stop.
		{stream: newStreamBuilder().AddStopWithUsage(1, 1).Build()},
	}}
	rt := newLiveSessionsRuntime(t, prov, mockModelStoreWithLimit{limit: 100_000})

	older := newWorkerSession("dup-id")
	newer := newWorkerSession("dup-id")

	streamA := rt.RunStream(t.Context(), older)
	waitClosed(t, startedA, "older stream turn")
	streamB := rt.RunStream(t.Context(), newer)
	waitClosed(t, startedB, "newer stream turn")

	// The registry now maps dup-id to the newer entry, so the request is
	// queued there.
	requestEvents := make(chan Event, 64)
	require.NoError(t, rt.CompactLiveSession(t.Context(), "dup-id", "", NewChannelSink(requestEvents)))

	// Run the older stream to completion (iteration boundary plus teardown
	// drain) while the request is still pending for the newer entry.
	close(releaseA)
	drainStream(t, streamA)

	assert.Empty(t, older.LastSummary(), "the older stream must not execute the newer entry's request")
	err := rt.CompactLiveSession(t.Context(), "dup-id", "", nil)
	require.Error(t, err, "the queued request must survive the older stream's boundary and teardown")
	assert.Contains(t, err.Error(), "already pending")

	close(releaseB)
	drainStream(t, streamB)
	close(requestEvents)

	var outcomes []string
	for ev := range requestEvents {
		if e, ok := ev.(*SessionCompactionEvent); ok && e.Status == "completed" {
			outcomes = append(outcomes, e.Outcome)
		}
	}
	assert.Equal(t, []string{CompactionOutcomeApplied}, outcomes)
	assert.Equal(t, "latest summary", newer.LastSummary(), "the request must compact the newer in-memory session")
	assert.Empty(t, older.LastSummary())
}

// TestCompactLiveSession_CancelledStreamEmitsSingleSkippedEvent pins the
// cancellation contract: a request accepted before the stream's context is
// cancelled is still consumed, but reports exactly one terminal
// completed/skipped event with the target's identity instead of attempting
// the compaction model call (which would emit a noisy started/failed pair).
func TestCompactLiveSession_CancelledStreamEmitsSingleSkippedEvent(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	prov := &stepProvider{id: "test/mock-model", steps: []providerStep{
		// The one and only turn, blocked until the context is cancelled.
		{stream: newStreamBuilder().AddStopWithUsage(1, 1).Build(), started: started, release: make(chan struct{})},
	}}
	rt := newLiveSessionsRuntime(t, prov, mockModelStoreWithLimit{limit: 100_000})

	ctx, cancel := context.WithCancel(t.Context())
	child := newWorkerSession("child-1")
	stream := rt.RunStream(ctx, child)
	waitClosed(t, started, "child turn")

	requestEvents := make(chan Event, 64)
	require.NoError(t, rt.CompactLiveSession(t.Context(), "child-1", "", NewChannelSink(requestEvents)))

	cancel()
	drainStream(t, stream)
	close(requestEvents)

	var kinds []string
	for ev := range requestEvents {
		if e, ok := ev.(*SessionCompactionEvent); ok {
			assert.Equal(t, "child-1", e.SessionID)
			assert.Equal(t, "worker", e.AgentName)
			kinds = append(kinds, e.Status+":"+e.Outcome)
		}
	}
	assert.Equal(t, []string{"completed:" + CompactionOutcomeSkipped}, kinds,
		"a cancelled target must consume the request and emit exactly one terminal skipped event")
	assert.Empty(t, child.LastSummary(), "no compaction must run against a cancelled stream")
}

// TestCompactLiveSession_HookVetoSynthesizesSkipped verifies that when a
// pre_compact hook vetoes the compaction (so no started/completed pair is
// emitted), the requester still observes a synthesized completed/skipped
// terminal event, mirroring App.CompactSession's root /compact behavior.
func TestCompactLiveSession_HookVetoSynthesizesSkipped(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})
	prov := &stepProvider{id: "test/mock-model", steps: []providerStep{
		{stream: newStreamBuilder().AddContent("working on it").Build(), started: started, release: release},
		{stream: newStreamBuilder().AddStopWithUsage(1, 1).Build()},
	}}
	rt := newLiveSessionsRuntime(t, prov, mockModelStoreWithLimit{limit: 100_000},
		agent.WithHooks(&latest.HooksConfig{
			PreCompact: []latest.HookDefinition{{Type: "builtin", Command: "test-veto-compact"}},
		}),
	)
	require.NoError(t, rt.hooksRegistry.RegisterBuiltin(
		"test-veto-compact",
		func(context.Context, *hooks.Input, []string) (*hooks.Output, error) {
			return &hooks.Output{Decision: hooks.DecisionBlockValue, Reason: "vetoed"}, nil
		},
	))

	child := newWorkerSession("child-1")
	stream := rt.RunStream(t.Context(), child)
	waitClosed(t, started, "first child turn")

	requestEvents := make(chan Event, 64)
	require.NoError(t, rt.CompactLiveSession(t.Context(), "child-1", "", NewChannelSink(requestEvents)))

	close(release)
	drainStream(t, stream)
	close(requestEvents)

	var statuses []string
	for ev := range requestEvents {
		if e, ok := ev.(*SessionCompactionEvent); ok {
			statuses = append(statuses, e.Status+":"+e.Outcome)
		}
	}
	assert.Equal(t, []string{"completed:" + CompactionOutcomeSkipped}, statuses,
		"a vetoed request must synthesize the terminal skipped event, with no started pair")
	assert.Empty(t, child.LastSummary(), "a vetoed compaction must not modify the session")
}

// TestLiveSessions_ConcurrentAccess exercises the registry under -race:
// listings and rejected targeting requests race with stream registration and
// teardown.
func TestLiveSessions_ConcurrentAccess(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	prov := &stepProvider{id: "test/mock-model", steps: []providerStep{
		{stream: newStreamBuilder().AddStopWithUsage(1, 1).Build(), release: release},
	}}
	rt := newLiveSessionsRuntime(t, prov, mockModelStoreWithLimit{limit: 1000})
	rootSess := session.New(session.WithID("root-session"), session.WithUserMessage("hi"))

	done := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-done:
					return
				default:
					rt.LiveSessions(t.Context(), rootSess)
					_ = rt.CompactLiveSession(t.Context(), "unknown", "", nil)
				}
			}
		})
	}

	child := newWorkerSession("child-1")
	stream := rt.RunStream(t.Context(), child)
	close(release)
	drainStream(t, stream)

	close(done)
	wg.Wait()
}
