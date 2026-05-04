package server

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/concurrent"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

// fakeObservableRuntime adds live-session observability capability on top of
// the minimal fakeRuntime used by the other SessionManager tests.
type fakeObservableRuntime struct {
	*fakeRuntime

	bus            *runtime.EventBus
	tree           []runtime.LiveSessionNode
	nodes          map[string]runtime.LiveSessionNode
	sessions       map[string]*session.Session
	steerCount     atomic.Int64
	followUpCount  atomic.Int64
	closeCount     atomic.Int64
	stopCount      atomic.Int64
	interruptCount atomic.Int64
	lastSteerMsg   atomic.Value // string
}

func newFakeObservableRuntime() *fakeObservableRuntime {
	return &fakeObservableRuntime{
		fakeRuntime: &fakeRuntime{streamDelay: 10 * time.Millisecond},
		bus:         runtime.NewEventBus(),
		nodes:       make(map[string]runtime.LiveSessionNode),
		sessions:    make(map[string]*session.Session),
	}
}

func (f *fakeObservableRuntime) SubscribeSession(ctx context.Context, sessionID string, buffer int) *runtime.Subscription {
	return f.bus.Subscribe(ctx, sessionID, buffer)
}

func (f *fakeObservableRuntime) LiveSessionTree(_ string) []runtime.LiveSessionNode {
	return f.tree
}

func (f *fakeObservableRuntime) LiveSessionNode(sessionID string) (runtime.LiveSessionNode, bool) {
	n, ok := f.nodes[sessionID]
	return n, ok
}

func (f *fakeObservableRuntime) LiveSession(sessionID string) (*session.Session, bool) {
	s, ok := f.sessions[sessionID]
	return s, ok
}

func (f *fakeObservableRuntime) SteerSessionByID(_ string, msg runtime.QueuedMessage) error {
	f.steerCount.Add(1)
	f.lastSteerMsg.Store(msg.Content)
	return nil
}

func (f *fakeObservableRuntime) FollowUpSessionByID(_ string, _ runtime.QueuedMessage) error {
	f.followUpCount.Add(1)
	return nil
}

func (f *fakeObservableRuntime) CloseSessionByID(_ string) error {
	f.closeCount.Add(1)
	return nil
}

func (f *fakeObservableRuntime) StopSessionByID(_ string) error {
	f.stopCount.Add(1)
	return nil
}

func (f *fakeObservableRuntime) InterruptSessionByID(_ string) error {
	f.interruptCount.Add(1)
	return nil
}

func newObservableSessionManager(t *testing.T, root *session.Session, fake *fakeObservableRuntime) *SessionManager {
	t.Helper()
	store := session.NewInMemorySessionStore()
	require.NoError(t, store.AddSession(t.Context(), root))

	rtMap := concurrent.NewMap[string, *activeRuntimes]()
	rtMap.Store(root.ID, &activeRuntimes{runtime: fake, session: root})

	return &SessionManager{
		runtimeSessions: rtMap,
		sessionStore:    store,
		Sources:         config.Sources{},
		runConfig:       &config.RuntimeConfig{},
	}
}

func TestSessionManager_LiveSessionTreeAndNode(t *testing.T) {
	root := session.New()
	fake := newFakeObservableRuntime()
	fake.tree = []runtime.LiveSessionNode{
		{SessionID: root.ID, RootSessionID: root.ID, Kind: runtime.LiveSessionRoot, Depth: 0, Status: "running", AgentName: "root"},
		{SessionID: "child-1", ParentSessionID: root.ID, RootSessionID: root.ID, Kind: runtime.LiveSessionSubAgent, Depth: 1, Status: "waiting", AgentName: "worker"},
	}
	fake.nodes["child-1"] = fake.tree[1]

	sm := newObservableSessionManager(t, root, fake)

	tree, err := sm.LiveSessionTree(root.ID)
	require.NoError(t, err)
	require.Len(t, tree, 2)
	assert.Equal(t, "child-1", tree[1].SessionID)

	node, err := sm.LiveSessionNode("child-1")
	require.NoError(t, err)
	assert.Equal(t, "worker", node.AgentName)
}

func TestSessionManager_AttachLiveSession(t *testing.T) {
	root := session.New()
	fake := newFakeObservableRuntime()
	fake.nodes[root.ID] = runtime.LiveSessionNode{SessionID: root.ID, RootSessionID: root.ID, Kind: runtime.LiveSessionRoot}

	sm := newObservableSessionManager(t, root, fake)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	sub, err := sm.AttachLiveSession(ctx, root.ID, 8)
	require.NoError(t, err)
	fake.bus.Publish(root.ID, runtime.Warning("hello", "root"))

	select {
	case ev := <-sub.Events:
		warn, ok := ev.(*runtime.WarningEvent)
		require.True(t, ok)
		assert.Equal(t, "hello", warn.Message)
	case <-time.After(time.Second):
		t.Fatal("expected live session event")
	}
}

func TestSessionManager_LiveSessionSnapshot(t *testing.T) {
	root := session.New(session.WithTitle("root"))
	root.AddMessage(session.UserMessage("hi"))
	fake := newFakeObservableRuntime()
	fake.tree = []runtime.LiveSessionNode{{SessionID: root.ID, RootSessionID: root.ID, Kind: runtime.LiveSessionRoot, AgentName: "root"}}
	fake.nodes[root.ID] = fake.tree[0]

	sm := newObservableSessionManager(t, root, fake)

	sess, node, err := sm.LiveSessionSnapshot(t.Context(), root.ID)
	require.NoError(t, err)
	assert.Same(t, root, sess, "root snapshot should hand back the runtime's session pointer")
	assert.Equal(t, root.ID, node.SessionID)

	child := session.New(session.WithAgentName("worker"), session.WithParentID(root.ID))
	child.AddMessage(session.UserMessage("reply"))
	fake.sessions[child.ID] = child
	fake.nodes[child.ID] = runtime.LiveSessionNode{SessionID: child.ID, ParentSessionID: root.ID, RootSessionID: root.ID, AgentName: "worker", Kind: runtime.LiveSessionSubAgent}

	gotSess, gotNode, err := sm.LiveSessionSnapshot(t.Context(), child.ID)
	require.NoError(t, err)
	assert.Same(t, child, gotSess, "descendant snapshot should hand back the subagent session pointer")
	assert.Equal(t, "worker", gotNode.AgentName)

	_, _, err = sm.LiveSessionSnapshot(t.Context(), "nope")
	require.Error(t, err)
}

func TestSessionManager_SteerAndFollowUpLiveSession(t *testing.T) {
	root := session.New()
	fake := newFakeObservableRuntime()
	// Register a child session so the control path finds it via the descendant
	// search rather than the root fast path.
	fake.nodes["child-1"] = runtime.LiveSessionNode{SessionID: "child-1", RootSessionID: root.ID, ParentSessionID: root.ID, Kind: runtime.LiveSessionSubAgent}

	sm := newObservableSessionManager(t, root, fake)

	ctx := t.Context()
	require.NoError(t, sm.SteerLiveSession(ctx, "child-1", []api.Message{{Content: "continue"}}))
	assert.EqualValues(t, 1, fake.steerCount.Load())
	assert.Equal(t, "continue", fake.lastSteerMsg.Load())

	require.NoError(t, sm.FollowUpLiveSession(ctx, "child-1", []api.Message{{Content: "later"}}))
	assert.EqualValues(t, 1, fake.followUpCount.Load())

	require.NoError(t, sm.CloseLiveSession(ctx, "child-1"))
	assert.EqualValues(t, 1, fake.closeCount.Load())

	require.NoError(t, sm.StopLiveSession(ctx, "child-1"))
	assert.EqualValues(t, 1, fake.stopCount.Load())

	require.NoError(t, sm.InterruptLiveSession(ctx, "child-1"))
	assert.EqualValues(t, 1, fake.interruptCount.Load())
}
