package runtime

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

func newRestoreFixture(t *testing.T) (*LocalRuntime, session.Store, *session.Session) {
	t.Helper()
	store, err := session.NewSQLiteSessionStore(t.Context(), filepath.Join(t.TempDir(), "s.db"))
	require.NoError(t, err)
	t.Cleanup(func() { store.(*session.SQLiteSessionStore).Close() })

	prov := func() *mockProvider {
		return &mockProvider{id: "test/mock-model", stream: newStreamBuilder().AddContent("ok").AddStopWithUsage(1, 1).Build()}
	}
	tm := team.New(team.WithAgents(
		agent.New("root", "prompt", agent.WithModel(prov())),
		agent.New("planner", "prompt", agent.WithModel(prov())),
	))
	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionStore(store))
	require.NoError(t, err)
	t.Cleanup(rt.subagents.Close)

	sess := session.New(session.WithID("parent-sess"))
	require.NoError(t, store.AddSession(t.Context(), sess))
	return rt, store, sess
}

func TestRestoreSubagentTreeResumesIdleSubagents(t *testing.T) {
	t.Parallel()

	rt, store, sess := newRestoreFixture(t)

	childSess := session.New(session.WithID("child-sess"))
	childSess.ParentID = sess.ID
	childSess.AddMessage(session.NewAgentMessage("planner", &chat.Message{Role: chat.MessageRoleAssistant, Content: "the plan"}))
	require.NoError(t, store.AddSession(t.Context(), childSess))
	stoppedSess := session.New(session.WithID("stopped-sess"))
	stoppedSess.ParentID = sess.ID
	require.NoError(t, store.AddSession(t.Context(), stoppedSess))
	stoppedChildSess := session.New(session.WithID("stopped-child-sess"))
	stoppedChildSess.ParentID = stoppedSess.ID
	require.NoError(t, store.AddSession(t.Context(), stoppedChildSess))

	rootID := subagent.SessionRootID(sess.ID)
	stored := subagent.Snapshot{Root: rootID, Nodes: []subagent.NodeSnapshot{{
		Node: subagent.Node{ID: rootID, Agent: "root", State: subagent.NodeRunning},
		Children: []subagent.NodeSnapshot{
			{Node: subagent.Node{ID: "77c88", Agent: "planner", Parent: rootID, SessionID: "child-sess", State: subagent.NodeRunning}},
			{Node: subagent.Node{ID: "00bad", Agent: "planner", Parent: rootID, State: subagent.NodeIdle}}, // no session id: unresumable
			{
				Node: subagent.Node{ID: "55d0f", Agent: "planner", Parent: rootID, SessionID: "stopped-sess", State: subagent.NodeStopped},
				Children: []subagent.NodeSnapshot{
					{Node: subagent.Node{ID: "c001d", Agent: "planner", Parent: "55d0f", SessionID: "stopped-child-sess", State: subagent.NodeRunning}},
				},
			},
		},
	}}}
	require.NoError(t, store.(*session.SQLiteSessionStore).SaveTree(t.Context(), sess.ID, stored))

	snap, err := rt.RestoreSubagentTree(t.Context(), sess)
	require.NoError(t, err)
	require.NotNil(t, snap)

	// The resumable child was adopted as idle (a run cannot survive a
	// restart) with its record rebuilt; the unresumable one is stopped.
	live, ok := rt.subagents.tree.Node("77c88")
	require.True(t, ok)
	assert.Equal(t, subagent.NodeIdle, live.State)
	rec, ok := rt.subagents.Read("77c88")
	require.True(t, ok)
	assert.Equal(t, subagent.NodeIdle, rec.state)
	assert.Equal(t, "the plan", rec.result, "latest result recovered from the transcript")

	dead, ok := rt.subagents.tree.Node("00bad")
	require.True(t, ok)
	assert.Equal(t, subagent.NodeStopped, dead.State)
	stopped, ok := rt.subagents.tree.Node("55d0f")
	require.True(t, ok)
	assert.Equal(t, subagent.NodeStopped, stopped.State, "persisted stopped subagents stay stopped even if resumable")
	stoppedRec, ok := rt.subagents.Read("55d0f")
	require.True(t, ok)
	assert.Equal(t, subagent.NodeStopped, stoppedRec.state)
	stoppedDescendant, ok := rt.subagents.tree.Node("c001d")
	require.True(t, ok)
	assert.Equal(t, subagent.NodeStopped, stoppedDescendant.State, "descendants of stopped subagents stay stopped")

	// Adopted subagents accept follow-ups; stopped ones don't.
	_, err = rt.subagents.sendToChild(sess.ID, "77c88", "continue please")
	require.NoError(t, err, "adopted subagents stay conversational")
	_, err = rt.subagents.sendToChild(sess.ID, "00bad", "hello?")
	require.Error(t, err)
	_, err = rt.subagents.sendToChild(sess.ID, "55d0f", "hello?")
	require.Error(t, err)

	// Idempotent for a session already tracked in-process.
	again, err := rt.RestoreSubagentTree(t.Context(), sess)
	require.NoError(t, err)
	require.NotNil(t, again)
}

// Attaching a viewer to an adopted subagent's session: injected input is
// mirrored as a UserMessageEvent (even while the child is idle) and the
// re-run's stream events follow.
func TestSessionEventsMirrorSubagentRuns(t *testing.T) {
	t.Parallel()

	rt, store, sess := newRestoreFixture(t)

	childSess := session.New(session.WithID("child-sess"))
	childSess.ParentID = sess.ID
	require.NoError(t, store.AddSession(t.Context(), childSess))

	rootID := subagent.SessionRootID(sess.ID)
	stored := subagent.Snapshot{Root: rootID, Nodes: []subagent.NodeSnapshot{{
		Node: subagent.Node{ID: rootID, Agent: "root", State: subagent.NodeRunning},
		Children: []subagent.NodeSnapshot{
			{Node: subagent.Node{ID: "77c88", Agent: "planner", Parent: rootID, SessionID: "child-sess", State: subagent.NodeIdle}},
		},
	}}}
	require.NoError(t, store.(*session.SQLiteSessionStore).SaveTree(t.Context(), sess.ID, stored))
	_, err := rt.RestoreSubagentTree(t.Context(), sess)
	require.NoError(t, err)

	info, ok := rt.SubagentAttachInfo("77c88")
	require.True(t, ok)
	assert.Equal(t, "planner", info.Agent)
	assert.Equal(t, sess.ID, info.ParentSessionID)
	assert.Equal(t, "root", info.ParentAgent)
	require.NotNil(t, info.Session)
	assert.Equal(t, "child-sess", info.Session.ID)

	_, events, cancel := rt.SubscribeSessionEvents("child-sess")
	defer cancel()

	require.True(t, rt.DeliverMessage(t.Context(), "child-sess", "keep going"))

	var sawUser, sawStop bool
	deadline := time.After(10 * time.Second)
	for !sawUser || !sawStop {
		select {
		case ev := <-events:
			switch e := ev.(type) {
			case *UserMessageEvent:
				if e.Message == "keep going" {
					sawUser = true
				}
			case *StreamStoppedEvent:
				sawStop = true
			}
		case <-deadline:
			t.Fatalf("timed out waiting for mirrored events (user=%v stop=%v)", sawUser, sawStop)
		}
	}
}

// After a reload, spawning a NEW subagent must not overwrite the persisted
// snapshot with a tree that only contains the new one: the adopted subagents
// have to survive the next persist (and therefore the next reload).
func TestRestoredSubagentsSurviveNewSpawns(t *testing.T) {
	t.Parallel()

	rt, store, sess := newRestoreFixture(t)

	childSess := session.New(session.WithID("child-sess"))
	childSess.ParentID = sess.ID
	require.NoError(t, store.AddSession(t.Context(), childSess))

	rootID := subagent.SessionRootID(sess.ID)
	stored := subagent.Snapshot{Root: rootID, Nodes: []subagent.NodeSnapshot{{
		Node: subagent.Node{ID: rootID, Agent: "root", State: subagent.NodeRunning},
		Children: []subagent.NodeSnapshot{
			{Node: subagent.Node{ID: "77c88", Agent: "planner", Parent: rootID, SessionID: "child-sess", State: subagent.NodeIdle}},
		},
	}}}
	require.NoError(t, store.(*session.SQLiteSessionStore).SaveTree(t.Context(), sess.ID, stored))

	_, err := rt.RestoreSubagentTree(t.Context(), sess)
	require.NoError(t, err)

	// Delegate to a new subagent after the reload.
	newID := rt.subagents.Spawn(sess, "root", subagent.AllowedSubagent{Agent: "planner"}, "new task")
	require.NotEmpty(t, newID)

	// The re-persisted snapshot must contain BOTH the adopted and the new
	// subagent.
	persisted, err := store.(*session.SQLiteSessionStore).LoadTree(t.Context(), sess.ID)
	require.NoError(t, err)
	require.NotNil(t, persisted)
	ids := map[subagent.NodeID]bool{}
	for _, root := range persisted.Nodes {
		if root.Node.ID == rootID {
			for _, c := range root.Children {
				ids[c.Node.ID] = true
			}
		}
	}
	assert.True(t, ids["77c88"], "adopted subagent survives the next persist")
	assert.True(t, ids[newID], "new subagent persisted alongside")
}

// A spawned subagent's session reads like an ordinary session: the task is
// the first regular user message (no task-in-system-prompt, no implicit
// "Please proceed."), and parent messages arrive as plain user messages.
// Transcript viewers (attached tabs, read_subagent) depend on this shape.
func TestSpawnedSubagentSessionShape(t *testing.T) {
	t.Parallel()

	rt, _, sess := newRestoreFixture(t)

	id := rt.subagents.Spawn(sess, "root", subagent.AllowedSubagent{Agent: "planner"}, "analyze the codebase")
	require.NotEmpty(t, id)

	info, ok := rt.SubagentAttachInfo(id)
	require.True(t, ok)

	msgs := info.Session.GetAllMessages()
	require.NotEmpty(t, msgs)
	first := msgs[0]
	assert.Equal(t, chat.MessageRoleUser, first.Message.Role, "the task is the first user message, not a system prompt")
	assert.Equal(t, "analyze the codebase", first.Message.Content)
	assert.False(t, first.Implicit, "the task must be visible to transcript viewers")
	for _, m := range msgs {
		assert.NotEqual(t, "Please proceed.", m.Message.Content, "no synthetic kick-off message")
		assert.NotEqual(t, chat.MessageRoleSystem, m.Message.Role,
			"zero session system messages: the child sees only what the parent wrote")
	}

	// A parent message lands as a plain user message — no system_info wrap.
	tc := tools.ToolCall{}
	tc.Function.Name = subagent.ToolSendMessage
	tc.Function.Arguments = `{"to":"` + string(id) + `","message":"extra context"}`
	res, err := rt.handleSendMessage(t.Context(), sess, tc, nil)
	require.NoError(t, err)
	require.False(t, res.IsError, res.Output)

	require.Eventually(t, func() bool {
		for _, m := range info.Session.GetAllMessages() {
			if m.Message.Role == chat.MessageRoleUser && m.Message.Content == "extra context" {
				return true
			}
		}
		return false
	}, 10*time.Second, 10*time.Millisecond, "parent message must reach the child session unwrapped")
}

// Child sessions get their title generated from the first user message (the
// task), like any other session — no hardcoded "Subagent <name>" label. The
// SessionTitleEvent reaches attached viewers through the session event hub.
func TestChildSessionTitleGeneration(t *testing.T) {
	t.Parallel()

	rt, _, _ := newRestoreFixture(t)

	child := session.New(session.WithID("child-title-sess"))
	_, events, cancel := rt.SubscribeSessionEvents(child.ID)
	defer cancel()

	rt.subagents.generateChildTitle(child, "analyze the codebase")

	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev := <-events:
			titleEv, ok := ev.(*SessionTitleEvent)
			if !ok {
				continue
			}
			assert.NotEmpty(t, titleEv.Title)
			assert.NotContains(t, titleEv.Title, "Subagent ", "no hardcoded label")
			// Publish happens after the title write: safe to read now.
			assert.Equal(t, titleEv.Title, child.Title)
			return
		case <-deadline:
			t.Fatal("timed out waiting for the child's SessionTitleEvent")
		}
	}
}

// Session links resolve at any depth: every subagent sub-session (including
// grandchildren) maps back to its node, and its attach info names its own
// parent session — what the TUI's "parent:" link relies on.
func TestSubagentNodeForSessionAtDepth(t *testing.T) {
	t.Parallel()

	rt, _, sess := newRestoreFixture(t)

	childID := rt.subagents.Spawn(sess, "root", subagent.AllowedSubagent{Agent: "planner"}, "child task")
	childInfo, ok := rt.SubagentAttachInfo(childID)
	require.True(t, ok)

	grandID := rt.subagents.Spawn(childInfo.Session, "planner", subagent.AllowedSubagent{Agent: "planner"}, "grandchild task")
	grandInfo, ok := rt.SubagentAttachInfo(grandID)
	require.True(t, ok)
	assert.Equal(t, childInfo.Session.ID, grandInfo.ParentSessionID, "grandchild's parent is the intermediate child session")

	// The intermediate session resolves to its node even with no tab open.
	node, ok := rt.SubagentNodeForSession(childInfo.Session.ID)
	require.True(t, ok)
	assert.Equal(t, childID, node)

	node, ok = rt.SubagentNodeForSession(grandInfo.Session.ID)
	require.True(t, ok)
	assert.Equal(t, grandID, node)

	_, ok = rt.SubagentNodeForSession("no-such-session")
	assert.False(t, ok)
}

// Startup info for a pinned session (e.g. an attached subagent tab) must
// describe the session's agent everywhere — including TeamInfoEvent's
// CurrentAgent, which the TUI uses as the selected agent.
func TestEmitStartupInfoHonoursPinnedSessionAgent(t *testing.T) {
	t.Parallel()

	rt, _, _ := newRestoreFixture(t)

	pinned := session.New(session.WithID("pinned-sess"), session.WithAgentName("planner"))
	events := make(chan Event, 64)
	go func() {
		defer close(events)
		rt.EmitStartupInfo(t.Context(), pinned, NewChannelSink(events))
	}()

	var agentName, teamCurrent string
	for ev := range events {
		switch e := ev.(type) {
		case *AgentInfoEvent:
			agentName = e.AgentName
		case *TeamInfoEvent:
			teamCurrent = e.CurrentAgent
		}
	}
	assert.Equal(t, "planner", agentName)
	assert.Equal(t, "planner", teamCurrent, "the selected agent is the session's pinned agent, not the runtime's global current agent")
}
