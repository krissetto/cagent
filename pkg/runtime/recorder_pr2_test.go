package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
)

func TestSessionRecorderPersistsUserMessagePositionallyIdempotent(t *testing.T) {
	store := session.NewInMemorySessionStore()
	sess := session.New(session.WithID("sess"))
	require.NoError(t, store.UpdateSession(t.Context(), sess))

	rec := NewSessionRecorder(store)
	t.Cleanup(rec.Close)
	rec.Handle(sess.ID, UserMessage("hello", sess.ID, nil, 0))
	rec.Handle(sess.ID, UserMessage("duplicate", sess.ID, nil, 0))
	rec.FlushSession(sess.ID)

	got, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Len(t, got.Messages, 1)
	assert.Equal(t, "hello", got.Messages[0].Message.Message.Content)
}

func TestSessionRecorderDoesNotDoubleWriteInMemoryRuntimeStore(t *testing.T) {
	store := session.NewInMemorySessionStore()
	r := &LocalRuntime{sessionStore: store, eventBus: NewEventBus(), liveSessions: newLiveSessionRegistry()}
	sess := session.New(session.WithID("sess"))
	sess.AddMessage(session.UserMessage("already-in-memory"))

	r.ensureSessionPersisted(t.Context(), sess)
	got, err := store.GetSession(t.Context(), sess.ID)
	require.ErrorIs(t, err, session.ErrNotFound)
	assert.Nil(t, got)
}

func TestSessionRecorderPersistsMessageAddedAtPosition(t *testing.T) {
	store := session.NewInMemorySessionStore()
	sess := session.New(session.WithID("sess"))
	require.NoError(t, store.UpdateSession(t.Context(), sess))

	rec := NewSessionRecorder(store)
	t.Cleanup(rec.Close)
	msg := session.UserMessage("final")
	rec.Handle(sess.ID, MessageAdded(sess.ID, msg, "agent", 0))
	rec.Handle(sess.ID, MessageAdded(sess.ID, session.UserMessage("duplicate"), "agent", 0))
	rec.FlushSession(sess.ID)

	got, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Len(t, got.Messages, 1)
	assert.Equal(t, "final", got.Messages[0].Message.Message.Content)
}
