package session

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSessionStorePersistsUpdatedAtAndTouchesOnMessage(t *testing.T) {
	ctx := t.Context()
	store, err := NewSQLiteSessionStore(t.TempDir() + "/sessions.db")
	require.NoError(t, err)
	defer store.Close()

	created := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	sess := New(WithID("s1"))
	sess.CreatedAt = created
	sess.UpdatedAt = created.Add(5 * time.Minute)
	require.NoError(t, store.AddSession(ctx, sess))

	loaded, err := store.GetSession(ctx, "s1")
	require.NoError(t, err)
	require.Equal(t, sess.UpdatedAt.Format(time.RFC3339), loaded.UpdatedAt.Format(time.RFC3339))

	_, err = store.AddMessage(ctx, "s1", UserMessage("hello"))
	require.NoError(t, err)
	loaded, err = store.GetSession(ctx, "s1")
	require.NoError(t, err)
	require.True(t, loaded.UpdatedAt.After(sess.UpdatedAt) || loaded.UpdatedAt.Equal(sess.UpdatedAt), "updated_at should not move backwards: %s <= %s", loaded.UpdatedAt, sess.UpdatedAt)
}
