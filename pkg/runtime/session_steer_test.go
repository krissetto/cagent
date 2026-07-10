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

func newDriverTestRuntime(t *testing.T) *LocalRuntime {
	t.Helper()
	r := &LocalRuntime{
		ctx:           func() context.Context { return context.WithoutCancel(t.Context()) },
		sessionEvents: newSessionEventHub(),
	}
	r.sessionDrivers = newSessionDriverRegistry(r)
	return r
}

func TestDeliverMessageSteersIntoLiveSessionLoop(t *testing.T) {
	t.Parallel()

	r := newDriverTestRuntime(t)
	sess := session.New(session.WithID("sess"))
	d := r.sessionDrivers.Get(sess)
	d.mu.Lock()
	d.running = true
	d.openSettledLocked()
	d.mu.Unlock()

	assert.True(t, r.deliverMessage(t.Context(), "sess", "note-1"))
	assert.True(t, r.deliverMessage(t.Context(), "sess", "note-2"))

	steered := r.drainSessionSteer("sess")
	require.Len(t, steered, 2)
	assert.Equal(t, "note-1", steered[0].Content)
	assert.Equal(t, "note-2", steered[1].Content)
	assert.Empty(t, r.drainSessionSteer("sess"), "drain consumes the buffer")
}

func TestDeliverMessageRequiresKnownSession(t *testing.T) {
	t.Parallel()

	r := newDriverTestRuntime(t)
	assert.False(t, r.deliverMessage(t.Context(), "sess", "strict"))
	assert.False(t, r.DeliverMessage(t.Context(), "sess", "strict"))
}

func TestDeliverOrBufferAdoptsNotesWhenSessionAppears(t *testing.T) {
	t.Parallel()

	r := newDriverTestRuntime(t)
	r.deliverOrBuffer(t.Context(), "sess", "turn-report")
	r.deliverOrBuffer(t.Context(), "sess", "child-message")

	sess := session.New(session.WithID("sess"))
	r.sessionDrivers.Get(sess)
	steered := r.drainSessionSteer("sess")
	require.Len(t, steered, 2)
	assert.Equal(t, "turn-report", steered[0].Content)
	assert.Equal(t, "child-message", steered[1].Content)
}

func TestDeliverMessageWakesKnownIdleSession(t *testing.T) {
	t.Parallel()

	rt, sess := newActorFixture(t)
	rt.rememberSession(sess)
	assert.True(t, rt.DeliverMessage(t.Context(), sess.ID, "fresh-turn"))
	assert.Eventually(t, func() bool {
		return rt.sessionSettled(sess.ID) && len(sess.Messages) >= 2
	}, 5*time.Second, 10*time.Millisecond)
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
