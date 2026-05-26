package session

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPublicRuntimeEventsInMemoryAppendReplayAndCursor(t *testing.T) {
	store := NewInMemorySessionStore().(PublicRuntimeEventStore)
	exercisePublicRuntimeEventStore(t, store)
}

func TestPublicRuntimeEventsSQLiteAppendReplayAndCursor(t *testing.T) {
	store := openMemoryStore(t)
	exercisePublicRuntimeEventStore(t, store)
}

func TestPublicRuntimeEventsSQLitePersistence(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "sessions.db")

	store, err := NewSQLiteSessionStore(path)
	require.NoError(t, err)
	require.NoError(t, store.AddSession(ctx, New(WithID("root"))))
	appended, err := store.(PublicRuntimeEventStore).AppendPublicRuntimeEvent(ctx, PublicRuntimeEvent{
		SessionID:   "root",
		RootID:      "root",
		Scope:       "session",
		Type:        "stream_started",
		PayloadJSON: `{"type":"stream_started"}`,
	})
	require.NoError(t, err)
	require.NoError(t, store.Close())

	reopened, err := NewSQLiteSessionStore(path)
	require.NoError(t, err)
	defer reopened.Close()
	events, err := reopened.(PublicRuntimeEventStore).ReplayPublicRuntimeEvents(ctx, PublicRuntimeEventQuery{RootID: "root"})
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, appended.EventID, events[0].EventID)
	assert.JSONEq(t, `{"type":"stream_started"}`, events[0].PayloadJSON)
}

func TestMigration023CreatesPublicRuntimeEventsTableAndIndexes(t *testing.T) {
	store := openMemoryStore(t)
	ctx := t.Context()

	for _, table := range []string{"public_runtime_events"} {
		var name string
		err := store.db.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name)
		require.NoError(t, err)
		assert.Equal(t, table, name)
	}
	for _, index := range []string{"idx_public_runtime_events_session_event", "idx_public_runtime_events_root_event"} {
		var name string
		err := store.db.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'index' AND name = ?`, index).Scan(&name)
		require.NoError(t, err)
		assert.Equal(t, index, name)
	}
}

func exercisePublicRuntimeEventStore(t *testing.T, publicStore PublicRuntimeEventStore) {
	t.Helper()
	ctx := t.Context()
	if store, ok := publicStore.(Store); ok {
		require.NoError(t, store.AddSession(ctx, New(WithID("root"))))
		require.NoError(t, store.AddSubSession(ctx, "root", New(WithID("child"))))
		require.NoError(t, store.AddSession(ctx, New(WithID("other"))))
	}

	first, err := publicStore.AppendPublicRuntimeEvent(ctx, PublicRuntimeEvent{
		SessionID:   "root",
		RootID:      "root",
		Scope:       "session",
		Type:        "stream_started",
		PayloadJSON: `{"type":"stream_started"}`,
	})
	require.NoError(t, err)
	second, err := publicStore.AppendPublicRuntimeEvent(ctx, PublicRuntimeEvent{
		SessionID:   "child",
		RootID:      "root",
		Scope:       "session",
		Type:        "agent_choice",
		PayloadJSON: `{"type":"agent_choice"}`,
	})
	require.NoError(t, err)
	third, err := publicStore.AppendPublicRuntimeEvent(ctx, PublicRuntimeEvent{
		SessionID:   "other",
		RootID:      "other",
		Scope:       "session",
		Type:        "stream_started",
		PayloadJSON: `{"type":"stream_started"}`,
	})
	require.NoError(t, err)

	assert.Positive(t, first.EventID)
	assert.Greater(t, second.EventID, first.EventID)
	assert.Greater(t, third.EventID, second.EventID)

	sessionEvents, err := publicStore.ReplayPublicRuntimeEvents(ctx, PublicRuntimeEventQuery{SessionID: "child"})
	require.NoError(t, err)
	require.Len(t, sessionEvents, 1)
	assert.Equal(t, second.EventID, sessionEvents[0].EventID)

	rootEvents, err := publicStore.ReplayPublicRuntimeEvents(ctx, PublicRuntimeEventQuery{RootID: "root"})
	require.NoError(t, err)
	require.Len(t, rootEvents, 2)
	assert.Equal(t, []int64{first.EventID, second.EventID}, []int64{rootEvents[0].EventID, rootEvents[1].EventID})

	afterEvents, err := publicStore.ReplayPublicRuntimeEvents(ctx, PublicRuntimeEventQuery{RootID: "root", AfterEventID: first.EventID})
	require.NoError(t, err)
	require.Len(t, afterEvents, 1)
	assert.Equal(t, second.EventID, afterEvents[0].EventID)
}
