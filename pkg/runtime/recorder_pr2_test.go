package runtime

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
)

func TestSessionRecorderPersistsUserMessagePositionallyIdempotent(t *testing.T) {
	store := session.NewInMemorySessionStore()
	sess := session.New(session.WithID("sess"))
	require.NoError(t, store.UpdateSession(t.Context(), sess))

	rec := NewSessionRecorder(store)
	t.Cleanup(rec.Close)
	rec.Handle(sess.ID, UserMessage("hello", sess.ID, nil, 0))
	rec.Handle(sess.ID, UserMessage("hello", sess.ID, nil, 0))
	rec.FlushSession(sess.ID)

	got, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Len(t, got.Messages, 1)
	assert.Equal(t, "hello", got.Messages[0].Message.Message.Content)
}

func TestSessionRecorderFollowUpPersistsAfterPositionGap(t *testing.T) {
	store := session.NewInMemorySessionStore()
	sess := session.New(session.WithID("sess"))
	require.NoError(t, store.UpdateSession(t.Context(), sess))

	rec := NewSessionRecorder(store)
	t.Cleanup(rec.Close)
	rec.Handle(sess.ID, UserMessage("follow-up after gap", sess.ID, nil, 3))
	rec.FlushSession(sess.ID)

	got, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Len(t, got.Messages, 1)
	assert.Equal(t, "follow-up after gap", got.Messages[0].Message.Message.Content)
}

func TestSessionRecorderFollowUpNotDroppedOnPositionCollision(t *testing.T) {
	store := session.NewInMemorySessionStore()
	sess := session.New(session.WithID("sess"))
	require.NoError(t, store.UpdateSession(t.Context(), sess))
	require.NoError(t, seedStoreMessages(t.Context(), store, sess.ID, "initial", "assistant reply"))

	rec := NewSessionRecorder(store)
	t.Cleanup(rec.Close)
	rec.Handle(sess.ID, UserMessage("follow-up colliding with assistant", sess.ID, nil, 1))
	rec.FlushSession(sess.ID)

	got, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Len(t, got.Messages, 3)
	assert.Equal(t, "initial", got.Messages[0].Message.Message.Content)
	assert.Equal(t, "assistant reply", got.Messages[1].Message.Message.Content)
	assert.Equal(t, "follow-up colliding with assistant", got.Messages[2].Message.Message.Content)
}

func TestSessionRecorderSubagentEnvelopeCollisionRemainsIdempotent(t *testing.T) {
	store := session.NewInMemorySessionStore()
	sess := session.New(session.WithID("sess"))
	require.NoError(t, store.UpdateSession(t.Context(), sess))

	rec := NewSessionRecorder(store)
	t.Cleanup(rec.Close)
	rec.Handle(sess.ID, TypedUserMessage(session.MessageKindSubagentEnvelope, "[worker] turn finished", sess.ID, nil, 0))
	rec.Handle(sess.ID, TypedUserMessage(session.MessageKindSubagentEnvelope, "[worker] turn finished", sess.ID, nil, 0))
	rec.FlushSession(sess.ID)

	got, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Len(t, got.Messages, 1)
	assert.Equal(t, session.MessageKindSubagentEnvelope, got.Messages[0].Message.Kind)
}

func TestSessionRecorderSQLiteReloadKeepsFollowUpAfterPositionGap(t *testing.T) {
	store, dbPath := newSQLiteRecorderStore(t)
	sess := session.New(session.WithID("sess"))
	require.NoError(t, store.AddSession(t.Context(), sess))

	rec := NewSessionRecorder(store)
	rec.Handle(sess.ID, UserMessage("initial prompt", sess.ID, nil, 0))
	rec.FlushSession(sess.ID)
	_, err := store.AddMessage(t.Context(), sess.ID, assistantMessage("first assistant reply"))
	require.NoError(t, err)

	rec.Handle(sess.ID, UserMessage("follow-up prompt", sess.ID, nil, 3))
	rec.FlushSession(sess.ID)
	_, err = store.AddMessage(t.Context(), sess.ID, assistantMessage("second assistant reply"))
	require.NoError(t, err)
	rec.Close()
	require.NoError(t, store.Close())

	reloaded := openSQLiteRecorderStore(t, dbPath)
	defer reloaded.Close()
	got, err := reloaded.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	assertMessageContents(t, got, "initial prompt", "first assistant reply", "follow-up prompt", "second assistant reply")
}

func TestSessionRecorderSQLiteReloadKeepsFollowUpAfterPositionCollision(t *testing.T) {
	store, dbPath := newSQLiteRecorderStore(t)
	sess := session.New(session.WithID("sess"))
	require.NoError(t, store.AddSession(t.Context(), sess))
	require.NoError(t, seedStoreMessages(t.Context(), store, sess.ID, "initial prompt", "first assistant reply"))

	rec := NewSessionRecorder(store)
	rec.Handle(sess.ID, UserMessage("follow-up prompt", sess.ID, nil, 1))
	rec.FlushSession(sess.ID)
	_, err := store.AddMessage(t.Context(), sess.ID, assistantMessage("second assistant reply"))
	require.NoError(t, err)
	rec.Close()
	require.NoError(t, store.Close())

	reloaded := openSQLiteRecorderStore(t, dbPath)
	defer reloaded.Close()
	got, err := reloaded.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	assertMessageContents(t, got, "initial prompt", "first assistant reply", "follow-up prompt", "second assistant reply")
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

func seedStoreMessages(ctx context.Context, store session.Store, sessionID, firstUser, assistantReply string) error {
	if _, err := store.AddMessage(ctx, sessionID, session.UserMessage(firstUser)); err != nil {
		return err
	}
	_, err := store.AddMessage(ctx, sessionID, assistantMessage(assistantReply))
	return err
}

func assistantMessage(content string) *session.Message {
	return &session.Message{
		AgentName: "root",
		Message:   chat.Message{Role: chat.MessageRoleAssistant, Content: content},
	}
}

func newSQLiteRecorderStore(t *testing.T) (session.Store, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	return openSQLiteRecorderStore(t, dbPath), dbPath
}

func openSQLiteRecorderStore(t *testing.T, dbPath string) session.Store {
	t.Helper()
	store, err := session.NewSQLiteSessionStore(dbPath)
	require.NoError(t, err)
	return store
}

func assertMessageContents(t *testing.T, sess *session.Session, want ...string) {
	t.Helper()
	require.Len(t, sess.Messages, len(want))
	for i, text := range want {
		require.NotNil(t, sess.Messages[i].Message, "message %d", i)
		assert.Equal(t, text, sess.Messages[i].Message.Message.Content)
	}
}
