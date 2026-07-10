package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

// newTestSubagentManager builds a manager bound to a minimal LocalRuntime so
// the receiver registry (r.receivers) is usable. It does not start any real
// sub-sessions.
func newTestSubagentManager(t *testing.T) *subagentManager {
	t.Helper()
	r := &LocalRuntime{
		receivers:     map[string]MessageReceiver{},
		subagentStore: subagent.NewInMemoryStore(),
		sessionEvents: newSessionEventHub(),
	}
	m := &subagentManager{
		r:        r,
		tree:     subagent.NewTree(),
		ctx:      t.Context(),
		sessions: map[string]*sessionSubagents{},
		children: map[subagent.NodeID]*childRecord{},
	}
	r.subagents = m
	return m
}

// registerChild wires a live child into the manager as Spawn would, without
// running a real sub-session.
func (m *subagentManager) registerChild(parent *session.Session, parentAgent string, id subagent.NodeID, name string, childSess *session.Session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.ensureSessionLocked(parent, parentAgent, "")
	st.live++
	_ = m.tree.Add(subagent.Node{ID: id, Agent: name, Parent: st.node, State: subagent.NodeRunning})
	m.ensureSessionLocked(childSess, name, id)
	m.children[id] = &childRecord{
		name:          name,
		parentSession: parent.ID,
		sessionID:     childSess.ID,
		session:       childSess,
		state:         subagent.NodeRunning,
	}
}

func TestSubagentManagerDeliversTurnReportToParentReceiver(t *testing.T) {
	m := newTestSubagentManager(t)
	parent := session.New(session.WithID("parent"))
	child := session.New(session.WithID("child"))
	m.registerChild(parent, "root", "aaaaa", "worker", child)

	// The parent's receiver captures delivered reports (like the TUI would).
	got := make(chan string, 1)
	m.r.RegisterMessageReceiver("parent", func(_ context.Context, content string) { got <- content })

	assert.True(t, m.HasLive("parent"))
	m.children["aaaaa"].result = "the answer is 42"
	m.children["aaaaa"].state = subagent.NodeIdle
	m.reportTurn("aaaaa", subagent.NodeIdle, "")

	select {
	case env := <-got:
		assert.Contains(t, env, "worker")
		assert.Contains(t, env, "finished its turn")
		// Short responses travel whole, explicitly marked as full.
		assert.Contains(t, env, `Full response: "the answer is 42"`)
		assert.NotContains(t, env, "[...]")
		assert.True(t, strings.HasPrefix(env, "<system_info>"), "report wrapped in system_info")
	case <-t.Context().Done():
		t.Fatal("parent receiver did not receive the turn report")
	}

	assert.True(t, m.HasLive("parent"), "reporting a turn keeps the subagent alive")

	require.NotNil(t, parent.SubagentTree, "tree snapshot mirrored onto the top-level session for live access")
	stored, err := m.r.subagentStore.LoadTree(t.Context(), "parent")
	require.NoError(t, err)
	require.NotNil(t, stored, "snapshot written to the subagent store on state change")
	found := false
	for _, root := range stored.Nodes {
		if root.Node.ID == subagent.SessionRootID("parent") {
			require.Len(t, root.Children, 1)
			assert.Equal(t, subagent.NodeIdle, root.Children[0].Node.State)
			found = true
		}
	}
	assert.True(t, found, "snapshot contains the parent session's root")

	rec, ok := m.Read("aaaaa")
	require.True(t, ok)
	assert.Equal(t, subagent.NodeIdle, rec.state)
	assert.Equal(t, "the answer is 42", rec.result)
}

func TestSubagentManagerSendToChildRoutesToChildReceiver(t *testing.T) {
	m := newTestSubagentManager(t)
	parent := session.New(session.WithID("parent"))
	child := session.New(session.WithID("child"))
	m.registerChild(parent, "root", "bbbbb", "worker", child)

	got := make(chan string, 1)
	m.r.RegisterMessageReceiver("child", func(_ context.Context, content string) { got <- content })

	name, err := m.sendToChild("parent", "bbbbb", "keep going")
	require.NoError(t, err)
	assert.Equal(t, "worker", name)
	select {
	case env := <-got:
		assert.Equal(t, "keep going", env)
	case <-t.Context().Done():
		t.Fatal("child receiver did not receive the message")
	}
}

func TestSubagentManagerSendToChildLifecycle(t *testing.T) {
	m := newTestSubagentManager(t)
	parent := session.New(session.WithID("parent"))
	child := session.New(session.WithID("child"))
	m.registerChild(parent, "root", "ccccc", "worker", child)
	m.r.RegisterMessageReceiver("child", func(context.Context, string) {})

	_, err := m.sendToChild("parent", "zzzzz", "hi")
	require.Error(t, err, "unknown id rejected")
	_, err = m.sendToChild("someone-else", "ccccc", "hi")
	require.Error(t, err, "foreign parent rejected")

	// A subagent that finished a turn stays conversational.
	m.children["ccccc"].result = "done"
	m.children["ccccc"].state = subagent.NodeIdle
	m.reportTurn("ccccc", subagent.NodeIdle, "")
	_, err = m.sendToChild("parent", "ccccc", "follow-up")
	require.NoError(t, err, "idle subagents accept follow-ups")

	// Only an explicit stop finalizes it.
	name, err := m.stopChild("parent", "ccccc")
	require.NoError(t, err)
	assert.Equal(t, "worker", name)
	_, err = m.sendToChild("parent", "ccccc", "hi")
	require.Error(t, err, "stopped subagent rejected")
	_, err = m.stopChild("parent", "ccccc")
	require.Error(t, err, "double stop rejected")
	assert.False(t, m.HasLive("parent"), "stop decrements the live count")

	// The record stays readable after the stop.
	rec, ok := m.Read("ccccc")
	require.True(t, ok)
	assert.Equal(t, subagent.NodeStopped, rec.state)
	assert.Equal(t, "done", rec.result)
}

func TestSubagentManagerDeepTreeLinkage(t *testing.T) {
	m := newTestSubagentManager(t)
	root := session.New(session.WithID("root-sess"))
	child := session.New(session.WithID("child-sess"))
	grandchild := session.New(session.WithID("gc-sess"))

	m.registerChild(root, "root", "ccccc", "coder", child)
	m.registerChild(child, "coder", "ddddd", "helper", grandchild)

	snap := m.tree.Snapshot()
	require.Len(t, snap.Nodes, 1, "single root")
	rootNode := snap.Nodes[0]
	require.Len(t, rootNode.Children, 1)
	childNode := rootNode.Children[0]
	assert.Equal(t, subagent.NodeID("ccccc"), childNode.Node.ID)
	require.Len(t, childNode.Children, 1, "grandchild linked under child, not a new root")
	assert.Equal(t, subagent.NodeID("ddddd"), childNode.Children[0].Node.ID)
}

func TestStopSubagentCascadesToDescendants(t *testing.T) {
	m := newTestSubagentManager(t)
	root := session.New(session.WithID("root-sess"))
	child := session.New(session.WithID("child-sess"))
	grandchild := session.New(session.WithID("gc-sess"))

	m.registerChild(root, "root", "ccccc", "coder", child)
	m.registerChild(child, "coder", "ddddd", "helper", grandchild)

	_, err := m.stopChild("root-sess", "ccccc")
	require.NoError(t, err)

	for _, id := range []subagent.NodeID{"ccccc", "ddddd"} {
		rec, ok := m.Read(id)
		require.True(t, ok)
		assert.Equal(t, subagent.NodeStopped, rec.state, "%s stopped", id)
		node, ok := m.tree.Node(id)
		require.True(t, ok)
		assert.Equal(t, subagent.NodeStopped, node.State, "%s tree state stopped", id)
	}
	_, err = m.sendToChild("child-sess", "ddddd", "hi")
	require.Error(t, err, "stopped descendant rejects input")
}

func TestDeliverMessageReturnsFalseWhenNoReceiver(t *testing.T) {
	m := newTestSubagentManager(t)
	assert.False(t, m.deliver("nobody", "hi"))
}

func TestRenderTranscriptLimit(t *testing.T) {
	sess := session.New(session.WithID("s"))
	sess.AddMessage(session.UserMessage("first"))
	sess.AddMessage(session.NewAgentMessage("worker", &chat.Message{Role: chat.MessageRoleAssistant, Content: "second"}))
	sess.AddMessage(session.NewAgentMessage("worker", &chat.Message{Role: chat.MessageRoleAssistant, Content: "third"}))

	full := renderTranscript(sess, 0)
	assert.Contains(t, full, "first")
	assert.Contains(t, full, "second")
	assert.Contains(t, full, "third")

	last := renderTranscript(sess, 1)
	assert.NotContains(t, last, "first")
	assert.Contains(t, last, "third")
}

// Long responses are previewed with a trailing "[...]" truncation marker (the
// signal that read_subagent has more); failures preview the error instead.
func TestTurnReportPreviews(t *testing.T) {
	newEnv := func(result, errMsg string, state subagent.NodeState) string {
		m := newTestSubagentManager(t)
		parent := session.New(session.WithID("parent"))
		child := session.New(session.WithID("child"))
		m.registerChild(parent, "root", "aaaaa", "worker", child)
		got := make(chan string, 1)
		m.r.RegisterMessageReceiver("parent", func(_ context.Context, content string) { got <- content })
		m.children["aaaaa"].result = result
		m.children["aaaaa"].state = state
		m.reportTurn("aaaaa", state, errMsg)
		select {
		case env := <-got:
			return env
		case <-t.Context().Done():
			t.Fatal("no report delivered")
			return ""
		}
	}

	t.Run("long response is truncated with marker", func(t *testing.T) {
		long := strings.Repeat("all work and no play ", 10)
		env := newEnv(long, "", subagent.NodeIdle)
		assert.Contains(t, env, "Full response preview:")
		assert.Contains(t, env, `[...]`)
		assert.NotContains(t, env, long, "the whole response must not be embedded")
	})

	t.Run("multiline response is collapsed to one line", func(t *testing.T) {
		env := newEnv("line one\nline two", "", subagent.NodeIdle)
		assert.Contains(t, env, `"line one line two"`)
	})

	t.Run("failed turn previews the error", func(t *testing.T) {
		env := newEnv("stale result", "model exploded", subagent.NodeFailed)
		assert.Contains(t, env, "failed")
		assert.Contains(t, env, `Error: "model exploded"`)
		assert.NotContains(t, env, "stale result")
	})

	t.Run("empty response omits the preview", func(t *testing.T) {
		env := newEnv("", "", subagent.NodeIdle)
		assert.Contains(t, env, "finished its turn")
		assert.NotContains(t, env, "response")
	})
}

// Quiescence gating: a subagent's turn end is reported to its parent only
// when no subagents of its own are still running — the delegation wave in a
// deep chain stays silent instead of waking every ancestor per leaf event.
// Failures always report.
func TestReportTurnQuiescenceGating(t *testing.T) {
	setup := func() (*subagentManager, chan string) {
		m := newTestSubagentManager(t)
		parent := session.New(session.WithID("parent"))
		child := session.New(session.WithID("child-sess"))
		grand := session.New(session.WithID("grand-sess"))
		m.registerChild(parent, "root", "aaaaa", "worker", child)
		m.registerChild(child, "worker", "bbbbb", "helper", grand)
		got := make(chan string, 4)
		m.r.RegisterMessageReceiver("parent", func(_ context.Context, content string) { got <- content })
		return m, got
	}

	t.Run("suppressed while its own subagent is running", func(t *testing.T) {
		m, got := setup()
		// helper (bbbbb) is running beneath worker; worker's turn end is
		// bookkeeping, not news.
		m.children["aaaaa"].state = subagent.NodeIdle
		m.reportTurn("aaaaa", subagent.NodeIdle, "")
		select {
		case env := <-got:
			t.Fatalf("parent must not be woken while the subtree works, got %q", env)
		default:
		}
		// The tree still records the state change for the UI.
		n, ok := m.tree.Node("aaaaa")
		require.True(t, ok)
		assert.Equal(t, subagent.NodeIdle, n.State)
	})

	t.Run("delivered once the subtree is quiet", func(t *testing.T) {
		m, got := setup()
		m.children["bbbbb"].state = subagent.NodeIdle // helper settled
		m.children["aaaaa"].state = subagent.NodeIdle
		m.children["aaaaa"].result = "all done"
		m.reportTurn("aaaaa", subagent.NodeIdle, "")
		select {
		case env := <-got:
			assert.Contains(t, env, "finished its turn")
			assert.Contains(t, env, "all done")
		default:
			t.Fatal("quiet subtree must report to the parent")
		}
	})

	t.Run("failures bypass the gate", func(t *testing.T) {
		m, got := setup()
		// helper still running, but worker's failure is always news.
		m.children["aaaaa"].state = subagent.NodeFailed
		m.reportTurn("aaaaa", subagent.NodeFailed, "model exploded")
		select {
		case env := <-got:
			assert.Contains(t, env, "failed")
			assert.Contains(t, env, "model exploded")
		default:
			t.Fatal("failures must always report")
		}
	})
}

// Spawning requires no embedder wiring: hosts that only call RunStream (API
// server, adapters, exec) still get working async subagents because the
// session actor wakes the parent for turn reports (see session_actor.go).
func TestSpawnToolNeedsNoReceiver(t *testing.T) {
	t.Parallel()

	tm := team.New(team.WithAgents(
		agent.New("root", "prompt",
			agent.WithModel(&mockProvider{id: "test/mock-model", stream: newStreamBuilder().AddContent("ok").AddStopWithUsage(1, 1).Build()}),
			agent.WithAsyncSubagents(latest.SubagentRef{Agent: "planner"})),
		agent.New("planner", "prompt",
			agent.WithModel(&mockProvider{id: "test/mock-model", stream: newStreamBuilder().AddContent("ok").AddStopWithUsage(1, 1).Build()})),
	))
	rt, err := NewLocalRuntime(t.Context(), tm)
	require.NoError(t, err)
	t.Cleanup(rt.subagents.Close)
	sess := session.New(session.WithID("parent-sess"))

	tc := tools.ToolCall{Function: tools.FunctionCall{
		Name:      subagent.ToolSpawnSubagent,
		Arguments: `{"agent":"planner","task":"analyze the codebase"}`,
	}}

	res, err := rt.handleSpawnSubagent(t.Context(), sess, tc, nil)
	require.NoError(t, err)
	assert.False(t, res.IsError, res.Output)
	assert.Contains(t, res.Output, "Spawned subagent")
}

func TestSpawnedSubagentInheritsSafetySettings(t *testing.T) {
	t.Parallel()

	tm := team.New(team.WithAgents(
		agent.New("root", "prompt",
			agent.WithModel(&mockProvider{id: "test/mock-model", stream: newStreamBuilder().AddContent("ok").AddStopWithUsage(1, 1).Build()}),
			agent.WithAsyncSubagents(latest.SubagentRef{Agent: "planner"})),
		agent.New("planner", "prompt",
			agent.WithModel(&mockProvider{id: "test/mock-model", stream: newStreamBuilder().AddContent("ok").AddStopWithUsage(1, 1).Build()})),
	))
	rt, err := NewLocalRuntime(t.Context(), tm)
	require.NoError(t, err)
	t.Cleanup(rt.subagents.Close)

	parent := session.New(
		session.WithID("parent-sess"),
		session.WithSafetyPolicy(session.SafetyPolicySafer),
		session.WithPermissions(&session.PermissionsConfig{Allow: []string{"shell"}}),
	)
	id := rt.subagents.Spawn(parent, "root", subagent.AllowedSubagent{Agent: "planner"}, "plan safely")
	info, ok := rt.SubagentAttachInfo(id)
	require.True(t, ok)
	assert.Equal(t, session.SafetyPolicySafer, info.Session.SafetyPolicy)
	require.NotNil(t, info.Session.Permissions)
	assert.Equal(t, []string{"shell"}, info.Session.Permissions.Allow)
}
