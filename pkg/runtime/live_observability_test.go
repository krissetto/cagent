package runtime

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
)

// ---------------------------------------------------------------------------
// liveSessionRegistry tests
// ---------------------------------------------------------------------------

func TestLiveRegistry_RegisterUnregisterGet(t *testing.T) {
	r := newLiveSessionRegistry()

	r.register("sess-1", "agentA", "")
	r.register("sess-2", "agentB", "sess-1")

	e1, ok := r.get("sess-1")
	if !ok || e1.agentName != "agentA" {
		t.Fatalf("expected to get sess-1 with agentA, got %+v ok=%v", e1, ok)
	}

	e2, ok := r.get("sess-2")
	if !ok || e2.parentID != "sess-1" {
		t.Fatalf("expected sess-2 with parentID=sess-1, got %+v ok=%v", e2, ok)
	}

	r.unregister("sess-1")
	_, ok = r.get("sess-1")
	if ok {
		t.Fatal("expected sess-1 to be removed after unregister")
	}

	// sess-2 remains.
	_, ok = r.get("sess-2")
	if !ok {
		t.Fatal("expected sess-2 to remain after unregistering sess-1")
	}
}

func TestLiveRegistry_All(t *testing.T) {
	r := newLiveSessionRegistry()
	r.register("a", "agentA", "")
	r.register("b", "agentB", "a")
	r.register("c", "agentC", "a")

	all := r.all()
	if len(all) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(all))
	}
}

func TestLiveRegistry_NilSafe(t *testing.T) {
	var r *liveSessionRegistry
	r.register("x", "a", "")
	r.unregister("x")
	_, ok := r.get("x")
	if ok {
		t.Fatal("expected nil registry get to return false")
	}
	if entries := r.all(); len(entries) != 0 {
		t.Fatalf("expected nil registry all to return empty slice, got %d", len(entries))
	}
}

func TestLiveRegistry_Idempotent(t *testing.T) {
	r := newLiveSessionRegistry()
	r.register("sess", "first", "")
	r.register("sess", "second", "parent") // overwrite
	e, ok := r.get("sess")
	if !ok || e.agentName != "second" || e.parentID != "parent" {
		t.Fatalf("expected overwritten entry second/parent, got %+v ok=%v", e, ok)
	}
}

// ---------------------------------------------------------------------------
// SessionTree tests
// ---------------------------------------------------------------------------

func TestSessionTree_NewAndRoot(t *testing.T) {
	root := LiveSessionNode{ID: "root", AgentName: "agent", Status: "running", Kind: LiveSessionRoot}
	tree := NewSessionTree(root, nil)
	if tree.Root().ID != "root" {
		t.Fatalf("expected root.ID=root, got %q", tree.Root().ID)
	}
}

func TestSessionTree_ChildrenAndSlice(t *testing.T) {
	t0 := time.Unix(1000, 0)
	t1 := time.Unix(2000, 0)
	t2 := time.Unix(3000, 0)

	root := LiveSessionNode{ID: "root", Status: "running", Kind: LiveSessionRoot, CreatedAt: t0}
	child1 := LiveSessionNode{ID: "c1", ParentID: "root", AgentName: "a1", Status: "running", Kind: LiveSessionSubAgent, CreatedAt: t1}
	child2 := LiveSessionNode{ID: "c2", ParentID: "root", AgentName: "a2", Status: "closed", Kind: LiveSessionSubAgent, CreatedAt: t2}
	grandchild := LiveSessionNode{ID: "gc1", ParentID: "c1", AgentName: "a3", Status: "running", Kind: LiveSessionSubAgent, CreatedAt: t1}

	tree := NewSessionTree(root, []LiveSessionNode{child1, child2, grandchild})

	// Children of root.
	rootChildren := tree.Children("root")
	if len(rootChildren) != 2 {
		t.Fatalf("expected 2 root children, got %d", len(rootChildren))
	}
	if rootChildren[0].ID != "c1" || rootChildren[1].ID != "c2" {
		t.Errorf("unexpected root child order: %v %v", rootChildren[0].ID, rootChildren[1].ID)
	}

	// Children of c1.
	c1Children := tree.Children("c1")
	if len(c1Children) != 1 || c1Children[0].ID != "gc1" {
		t.Errorf("expected c1 to have gc1, got %v", c1Children)
	}

	// DFS slice: root, c1, gc1, c2.
	slice := tree.Slice()
	ids := make([]string, len(slice))
	for i, n := range slice {
		ids[i] = n.ID
	}
	expected := []string{"root", "c1", "gc1", "c2"}
	for i, wantID := range expected {
		if ids[i] != wantID {
			t.Errorf("slice[%d]: want %q got %q (full=%v)", i, wantID, ids[i], ids)
		}
	}

	// Children field populated in Slice output.
	for _, n := range slice {
		if n.ID == "root" {
			if len(n.Children) != 2 {
				t.Errorf("root.Children should be 2, got %d", len(n.Children))
			}
		}
	}
}

func TestSessionTree_NodeLookup(t *testing.T) {
	root := LiveSessionNode{ID: "root", Status: "running"}
	child := LiveSessionNode{ID: "child", ParentID: "root", Status: "running"}
	tree := NewSessionTree(root, []LiveSessionNode{child})

	n, ok := tree.Node("child")
	if !ok || n.ID != "child" {
		t.Fatalf("expected to find child node, got %+v ok=%v", n, ok)
	}
	_, ok = tree.Node("missing")
	if ok {
		t.Fatal("expected missing node to return ok=false")
	}
}

func TestSessionTree_EmptyRoot(t *testing.T) {
	tree := NewSessionTree(LiveSessionNode{}, nil)
	if tree.Root().ID != "" {
		t.Fatal("expected empty root ID")
	}
	// Slice of empty tree should be empty (root ID is "" so no node added).
	slice := tree.Slice()
	if len(slice) != 0 {
		t.Fatalf("expected empty slice for empty root, got %d", len(slice))
	}
}

// ---------------------------------------------------------------------------
// LiveEventSource tests
// ---------------------------------------------------------------------------

func TestAttachLiveSession_ReceivesPublishedEvents(t *testing.T) {
	bus := NewEventBus()
	sessionID := "sess-attach"

	ch, cancel := attachLiveSessionViaEventBus(bus, t.Context(), sessionID)
	defer cancel()

	ev := StreamStarted(sessionID, "agent")
	bus.Publish(sessionID, ev)

	select {
	case got := <-ch:
		if got != ev {
			t.Errorf("unexpected event: %v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for event")
	}
}

// attachLiveSessionViaEventBus mirrors AttachLiveSession without requiring a
// full LocalRuntime — useful for unit testing the EventBus path directly.
func attachLiveSessionViaEventBus(bus *EventBus, ctx context.Context, sessionID string) (<-chan Event, func()) {
	sub := bus.Subscribe(ctx, sessionID, 32)
	return sub.Events, sub.Cancel
}

func TestAttachLiveSession_CancelClosesChannel(t *testing.T) {
	bus := NewEventBus()
	ch, cancel := attachLiveSessionViaEventBus(bus, t.Context(), "sess-cancel")
	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected channel to be closed after cancel")
		}
	case <-time.After(time.Second):
		t.Fatal("timeout: channel not closed after cancel")
	}
}

// ---------------------------------------------------------------------------
// AttachLiveSessionWithSnapshot (synthesize events from store)
// ---------------------------------------------------------------------------

func TestSynthesizeSnapshotEvents_UserAndAssistant(t *testing.T) {
	sess := session.New()
	userMsg := session.UserMessage("hello world")
	sess.AddMessage(userMsg)
	assistantMsg := &session.Message{
		AgentName: "agent",
		Message: chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: "hi there",
		},
	}
	sess.AddMessage(assistantMsg)

	evts := synthesizeSnapshotEvents(sess)
	if len(evts) != 2 {
		t.Fatalf("expected 2 events, got %d", len(evts))
	}
	userEv, ok := evts[0].(*UserMessageEvent)
	if !ok {
		t.Fatalf("expected UserMessageEvent, got %T", evts[0])
	}
	if userEv.Message != "hello world" {
		t.Errorf("unexpected user message: %q", userEv.Message)
	}
	if userEv.SessionPosition != 0 {
		t.Errorf("expected position 0, got %d", userEv.SessionPosition)
	}
	msgAdded, ok := evts[1].(*MessageAddedEvent)
	if !ok {
		t.Fatalf("expected MessageAddedEvent, got %T", evts[1])
	}
	if msgAdded.SessionPosition != 1 {
		t.Errorf("expected position 1, got %d", msgAdded.SessionPosition)
	}
}

func TestSynthesizeSnapshotEvents_Summary(t *testing.T) {
	sess := session.New()
	sess.Messages = []session.Item{
		{Summary: "a compact summary", FirstKeptEntry: 0},
	}
	evts := synthesizeSnapshotEvents(sess)
	if len(evts) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evts))
	}
	if _, ok := evts[0].(*SessionSummaryEvent); !ok {
		t.Fatalf("expected SessionSummaryEvent, got %T", evts[0])
	}
}

func TestSynthesizeSnapshotEvents_Nil(t *testing.T) {
	if evts := synthesizeSnapshotEvents(nil); len(evts) != 0 {
		t.Fatalf("expected empty for nil session, got %d", len(evts))
	}
}

// ---------------------------------------------------------------------------
// observability.LiveSessionTree tests
// ---------------------------------------------------------------------------

func TestLiveSessionTree_EmptyRuntime(t *testing.T) {
	rt := newMinimalRuntime(t)
	tree, err := rt.LiveSessionTree("sess-root")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tree.Root().ID != "sess-root" {
		t.Errorf("expected root ID sess-root, got %q", tree.Root().ID)
	}
	// No descendants — slice should have exactly the root.
	slice := tree.Slice()
	if len(slice) != 1 {
		t.Fatalf("expected 1-element slice (just root), got %d", len(slice))
	}
}

func TestLiveSessionTree_WithRegistry(t *testing.T) {
	rt := newMinimalRuntime(t)
	rt.liveSessions.register("sess-root", "agentA", "")

	tree, err := rt.LiveSessionTree("sess-root")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	root := tree.Root()
	if root.AgentName != "agentA" {
		t.Errorf("expected agentA from registry, got %q", root.AgentName)
	}
}

func TestLiveSessionTree_WithPersistedDescendants(t *testing.T) {
	store := session.NewInMemorySessionStore()
	parentSess := session.New(session.WithID("sess-root"))
	childSess := session.New(session.WithID("sess-child"), session.WithParentID("sess-root"), session.WithAgentName("reviewer"))
	ctx := t.Context()

	if err := store.AddSession(ctx, parentSess); err != nil {
		t.Fatal(err)
	}
	if err := store.AddSession(ctx, childSess); err != nil {
		t.Fatal(err)
	}

	rt := newMinimalRuntime(t)
	rt.sessionStore = store

	tree, err := rt.LiveSessionTree("sess-root")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	slice := tree.Slice()
	// Expect root + child.
	if len(slice) != 2 {
		names := make([]string, len(slice))
		for i, n := range slice {
			names[i] = fmt.Sprintf("%s(%s)", n.ID, n.Status)
		}
		t.Fatalf("expected 2 nodes, got %d: %v", len(slice), names)
	}
	if slice[1].ID != "sess-child" {
		t.Errorf("expected child node, got %q", slice[1].ID)
	}
	if slice[1].Status != "closed" {
		t.Errorf("expected closed status for persisted child, got %q", slice[1].Status)
	}
	if slice[1].AgentName != "reviewer" {
		t.Errorf("expected agent name 'reviewer' for persisted child, got %q", slice[1].AgentName)
	}
}

// ---------------------------------------------------------------------------
// Observability control methods tests
// ---------------------------------------------------------------------------

func TestSteerSessionByID_NotInTree(t *testing.T) {
	rt := newMinimalRuntime(t)
	err := rt.SteerSessionByID("unknown-session", QueuedMessage{Content: "hello"})
	if !errors.Is(err, ErrSessionNotInTree) {
		t.Errorf("expected ErrSessionNotInTree, got %v", err)
	}
}

func TestFollowUpSessionByID_NotInTree(t *testing.T) {
	rt := newMinimalRuntime(t)
	err := rt.FollowUpSessionByID("unknown", QueuedMessage{Content: "x"})
	if !errors.Is(err, ErrSessionNotInTree) {
		t.Errorf("expected ErrSessionNotInTree, got %v", err)
	}
}

func TestInterruptSessionByID_NotInTree(t *testing.T) {
	rt := newMinimalRuntime(t)
	err := rt.InterruptSessionByID("unknown")
	if !errors.Is(err, ErrSessionNotInTree) {
		t.Errorf("expected ErrSessionNotInTree, got %v", err)
	}
}

func TestCloseSessionByID_NotInTree(t *testing.T) {
	rt := newMinimalRuntime(t)
	err := rt.CloseSessionByID("unknown")
	if !errors.Is(err, ErrSessionNotInTree) {
		t.Errorf("expected ErrSessionNotInTree, got %v", err)
	}
}

func TestStopSessionByID_NotInTree(t *testing.T) {
	rt := newMinimalRuntime(t)
	err := rt.StopSessionByID("unknown")
	if !errors.Is(err, ErrSessionNotInTree) {
		t.Errorf("expected ErrSessionNotInTree, got %v", err)
	}
}

func TestSteerSessionByID_RootSession(t *testing.T) {
	rt := newMinimalRuntime(t)
	rt.liveSessions.register("sess-root", "agentA", "")

	// Should route to the runtime steer queue (this calls r.Steer).
	err := rt.SteerSessionByID("sess-root", QueuedMessage{Content: "steer msg"})
	if err != nil {
		t.Errorf("unexpected error steering root session: %v", err)
	}
	// Drain queue to confirm enqueued.
	msgs := rt.steerQueue.Drain(t.Context())
	if len(msgs) != 1 || msgs[0].Content != "steer msg" {
		t.Errorf("expected steer msg in queue, got %v", msgs)
	}
}

func TestFollowUpSessionByID_RootSession(t *testing.T) {
	rt := newMinimalRuntime(t)
	rt.liveSessions.register("sess-root", "agentA", "")

	err := rt.FollowUpSessionByID("sess-root", QueuedMessage{Content: "follow"})
	if err != nil {
		t.Errorf("unexpected error following up root session: %v", err)
	}
	msg, ok := rt.followUpQueue.Dequeue(t.Context())
	if !ok || msg.Content != "follow" {
		t.Errorf("expected follow-up in queue, got ok=%v msg=%v", ok, msg)
	}
}

// ---------------------------------------------------------------------------
// Compile-time interface assertion (tested at build time already)
// ---------------------------------------------------------------------------

var _ LiveSessionRuntime = (*LocalRuntime)(nil)

var _ LiveEventSource = (*LocalRuntime)(nil)

var _ LiveEventSourceWithSnapshot = (*LocalRuntime)(nil)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// newMinimalRuntime builds the minimal LocalRuntime needed for observability
// tests without a real LLM model, using the team-based constructor.
func newMinimalRuntime(t *testing.T) *LocalRuntime {
	t.Helper()
	rt := &LocalRuntime{
		steerQueue:    NewInMemoryMessageQueue(defaultSteerQueueCapacity),
		followUpQueue: NewInMemoryMessageQueue(defaultFollowUpQueueCapacity),
		liveSessions:  newLiveSessionRegistry(),
		eventBus:      NewEventBus(),
	}
	return rt
}
