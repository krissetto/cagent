package runtime

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/team"
)

func TestDeliverMessageSteersIntoLiveSessionLoop(t *testing.T) {
	t.Parallel()

	r := &LocalRuntime{receivers: map[string]MessageReceiver{}}
	received := false
	r.RegisterMessageReceiver("sess", func(context.Context, string) { received = true })

	r.beginSessionRun("sess")
	assert.True(t, r.deliverMessage(t.Context(), "sess", "note-1"))
	assert.True(t, r.deliverMessage(t.Context(), "sess", "note-2"))
	assert.False(t, received, "live loop must be steered, not re-run via the receiver")

	steered := r.drainSessionSteer("sess")
	require.Len(t, steered, 2)
	assert.Equal(t, "note-1", steered[0].Content)
	assert.Equal(t, "note-2", steered[1].Content)
	assert.Empty(t, r.drainSessionSteer("sess"), "drain consumes the buffer")
}

func TestDeliverMessageUsesReceiverWhenSessionIdle(t *testing.T) {
	t.Parallel()

	r := &LocalRuntime{receivers: map[string]MessageReceiver{}}
	got := make(chan string, 1)
	r.RegisterMessageReceiver("sess", func(_ context.Context, content string) { got <- content })

	assert.True(t, r.deliverMessage(t.Context(), "sess", "fresh-turn"))
	select {
	case content := <-got:
		assert.Equal(t, "fresh-turn", content)
	default:
		t.Fatal("idle session should deliver through the receiver")
	}
	assert.Empty(t, r.drainSessionSteer("sess"))
}

func TestEndSessionRunRedispatchesStrandedMessages(t *testing.T) {
	t.Parallel()

	r := &LocalRuntime{receivers: map[string]MessageReceiver{}}
	got := make(chan string, 2)
	r.RegisterMessageReceiver("sess", func(_ context.Context, content string) { got <- content })

	// A message buffered after the loop's final steer drain but before
	// teardown must not strand: endSessionRun re-routes it via the receiver.
	r.beginSessionRun("sess")
	require.True(t, r.deliverMessage(t.Context(), "sess", "raced-with-teardown"))
	r.endSessionRun(t.Context(), "sess")

	select {
	case content := <-got:
		assert.Equal(t, "raced-with-teardown", content)
	case <-time.After(2 * time.Second):
		t.Fatal("stranded message was not re-dispatched")
	}
	assert.Empty(t, r.drainSessionSteer("sess"))
}

// Notes to a session whose receiver is temporarily gone (owning tab switched
// away, teardown race) are buffered, not dropped, and handed over the moment
// a wake path reappears: the next run's opening steer drain, or — tested
// here last — a receiver re-registration.
func TestDeliverOrBufferKeepsNotesForReceiverlessSessions(t *testing.T) {
	t.Parallel()

	r := &LocalRuntime{ctx: t.Context}

	// Strict delivery reports the truth: no loop, no receiver.
	assert.False(t, r.deliverMessage(t.Context(), "sess", "strict"))
	assert.Empty(t, r.drainSessionSteer("sess"))

	// deliverOrBuffer holds the note for the next run instead.
	r.deliverOrBuffer(t.Context(), "sess", "turn-report")
	r.deliverOrBuffer(t.Context(), "sess", "child-message")
	steered := r.drainSessionSteer("sess")
	require.Len(t, steered, 2)
	assert.Equal(t, "turn-report", steered[0].Content)
	assert.Equal(t, "child-message", steered[1].Content)

	// Teardown re-buffering: a note raced with the loop's end returns to the
	// buffer when no receiver can start a fresh run.
	r.beginSessionRun("sess")
	require.True(t, r.deliverMessage(t.Context(), "sess", "raced-with-teardown"))
	r.endSessionRun(t.Context(), "sess")
	assert.Eventually(t, func() bool {
		r.sessionRunsMu.Lock()
		defer r.sessionRunsMu.Unlock()
		return len(r.sessionSteer["sess"]) == 1
	}, 2*time.Second, 10*time.Millisecond, "teardown must re-buffer, not drop")

	// With a receiver present, buffered semantics are unchanged: idle
	// delivery goes through it immediately.
	got := make(chan string, 1)
	unregister := r.RegisterMessageReceiver("sess2", func(_ context.Context, content string) { got <- content })
	defer unregister()
	r.deliverOrBuffer(t.Context(), "sess2", "instant")
	assert.Equal(t, "instant", <-got)
	assert.Empty(t, r.drainSessionSteer("sess2"))

	// A wake path reappearing drains what accumulated in its absence: the
	// re-buffered teardown note above reaches the new receiver without
	// waiting for the session's next run.
	reRegistered := make(chan string, 1)
	unregister2 := r.RegisterMessageReceiver("sess", func(_ context.Context, content string) { reRegistered <- content })
	defer unregister2()
	select {
	case content := <-reRegistered:
		assert.Equal(t, "raced-with-teardown", content)
	case <-time.After(2 * time.Second):
		t.Fatal("registering a receiver must drain buffered notes")
	}
	assert.Empty(t, r.drainSessionSteer("sess"))
}

func TestEndSessionRunKeepsBufferWhileAnotherRunLive(t *testing.T) {
	t.Parallel()

	r := &LocalRuntime{receivers: map[string]MessageReceiver{}}
	received := false
	r.RegisterMessageReceiver("sess", func(context.Context, string) { received = true })

	r.beginSessionRun("sess")
	r.beginSessionRun("sess")
	require.True(t, r.deliverMessage(t.Context(), "sess", "for-the-survivor"))
	r.endSessionRun(t.Context(), "sess")

	assert.False(t, received, "buffer belongs to the still-live loop")
	steered := r.drainSessionSteer("sess")
	require.Len(t, steered, 1)
	assert.Equal(t, "for-the-survivor", steered[0].Content)
	r.endSessionRun(t.Context(), "sess")
}

func TestSubagentDeliverSteersBusyParentInsteadOfQueueing(t *testing.T) {
	t.Parallel()

	m := newTestSubagentManager(t)
	parent := session.New(session.WithID("parent"))
	child := session.New(session.WithID("child"))
	m.registerChild(parent, "root", "bb2cc", "worker", child)

	// The parent's receiver stands in for the TUI SendMsg path, which would
	// surface the report in the user-visible queue — exactly what a busy
	// parent must avoid.
	viaReceiver := make(chan string, 1)
	m.r.RegisterMessageReceiver("parent", func(_ context.Context, content string) { viaReceiver <- content })

	m.r.beginSessionRun("parent")
	defer m.r.endSessionRun(t.Context(), "parent")

	m.children["bb2cc"].result = "done"
	m.children["bb2cc"].state = subagent.NodeIdle
	m.reportTurn("bb2cc", subagent.NodeIdle, "")

	select {
	case content := <-viaReceiver:
		t.Fatalf("report reached the receiver (TUI queue) while the parent loop was live: %q", content)
	default:
	}
	steered := m.r.drainSessionSteer("parent")
	require.Len(t, steered, 1)
	assert.Contains(t, steered[0].Content, "worker")
	assert.Contains(t, steered[0].Content, "finished its turn")
}

// The runtime derives its subagent store from the session store: SQLite-backed
// session stores persist swarm snapshots in their own table; anything else
// falls back to in-memory. Both are overridable via WithSubagentStore.
func TestSubagentStoreDefaulting(t *testing.T) {
	t.Parallel()

	tm := team.New(team.WithAgents(agent.New("root", "prompt",
		agent.WithModel(&mockProvider{id: "test/mock-model"}))))

	sqlStore, err := session.NewSQLiteSessionStore(t.Context(), filepath.Join(t.TempDir(), "s.db"))
	require.NoError(t, err)
	defer sqlStore.(*session.SQLiteSessionStore).Close()

	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionStore(sqlStore))
	require.NoError(t, err)
	assert.Same(t, sqlStore, rt.subagentStore, "session store implementing subagent.Store is reused")

	rt, err = NewLocalRuntime(t.Context(), tm)
	require.NoError(t, err)
	assert.IsType(t, &subagent.InMemoryStore{}, rt.subagentStore, "in-memory fallback when the session store cannot persist trees")

	custom := subagent.NewInMemoryStore()
	rt, err = NewLocalRuntime(t.Context(), tm, WithSessionStore(sqlStore), WithSubagentStore(custom))
	require.NoError(t, err)
	assert.Same(t, custom, rt.subagentStore, "explicit WithSubagentStore wins")
}
