package session

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewRuntimeManagedSubSession(t *testing.T) {
	root := New(WithID("root"))
	child := NewRuntimeManagedSubSession(root, WithID("child"))
	nested := NewRuntimeManagedSubSession(child, WithID("nested"))

	assert.Equal(t, "root", root.EffectiveRootID())
	assert.Equal(t, "root", child.ParentID)
	assert.Equal(t, "root", child.RootID)
	assert.True(t, child.RuntimeManaged)
	assert.Equal(t, "child", nested.ParentID)
	assert.Equal(t, "root", nested.RootID)
	assert.True(t, nested.RuntimeManaged)
}

func TestNewRuntimeManagedSubSessionPanicsOnNilParent(t *testing.T) {
	assert.PanicsWithValue(t, "session: nil parent for runtime-managed sub-session", func() {
		NewRuntimeManagedSubSession(nil, WithID("child"))
	})
}

func TestSubagentEnvelopeMessage(t *testing.T) {
	msg := SubagentEnvelopeMessage("subagent says hi")

	require.NotNil(t, msg)
	assert.True(t, msg.Implicit)
	assert.Equal(t, MessageKindSubagentEnvelope, msg.Kind)
	assert.Equal(t, "subagent says hi", msg.Message.Content)
	assert.Equal(t, "user", string(msg.Message.Role))
}

func TestTopologyStoresParity(t *testing.T) {
	t.Run("in-memory", func(t *testing.T) {
		store := NewInMemorySessionStore()
		assertTopologyStore(t, store, store.(TreeStore))
	})

	t.Run("sqlite", func(t *testing.T) {
		store := openMemorySQLiteStore(t)
		assertTopologyStore(t, store, store)
	})
}

func assertTopologyStore(t *testing.T, store Store, treeStore TreeStore) {
	t.Helper()
	ctx := t.Context()
	root := New(WithID("root"), WithTitle("root"))
	child := NewRuntimeManagedSubSession(root, WithID("child"), WithTitle("child"))
	nested := NewRuntimeManagedSubSession(child, WithID("nested"), WithTitle("nested"))

	require.NoError(t, store.UpdateSession(ctx, root))
	require.NoError(t, store.UpdateSession(ctx, child))
	require.NoError(t, store.UpdateSession(ctx, nested))

	children, err := treeStore.GetChildSessions(ctx, root.ID)
	require.NoError(t, err)
	require.Len(t, children, 1)
	assert.Equal(t, child.ID, children[0].ID)
	assert.Equal(t, root.ID, children[0].RootID)

	tree, err := treeStore.GetSessionTree(ctx, root.ID)
	require.NoError(t, err)
	require.Len(t, tree, 3)
	assert.Equal(t, []string{"root", "child", "nested"}, []string{tree[0].ID, tree[1].ID, tree[2].ID})

	resolved, err := treeStore.ResolveRootID(ctx, nested.ID)
	require.NoError(t, err)
	assert.Equal(t, root.ID, resolved)
}

func TestAddMessageAtIdempotentStoresParity(t *testing.T) {
	t.Run("in-memory", func(t *testing.T) {
		store := NewInMemorySessionStore()
		assertAddMessageAtIdempotent(t, store, store.(PositionalStore))
	})

	t.Run("sqlite", func(t *testing.T) {
		store := openMemorySQLiteStore(t)
		assertAddMessageAtIdempotent(t, store, store)
	})
}

func assertAddMessageAtIdempotent(t *testing.T, store Store, posStore PositionalStore) {
	t.Helper()
	ctx := t.Context()
	session := New(WithID("positional"))
	require.NoError(t, store.UpdateSession(ctx, session))

	original := SubagentEnvelopeMessage("hello")
	id1, err := posStore.AddMessageAt(ctx, session.ID, 0, original)
	require.NoError(t, err)
	require.NotZero(t, id1)
	assert.Zero(t, original.ID, "AddMessageAt must not mutate caller message")

	id2, err := posStore.AddMessageAt(ctx, session.ID, 0, UserMessage("hello again"))
	require.NoError(t, err)
	assert.Zero(t, id2, "duplicate insert at same position should be ignored")

	_, err = posStore.AddMessageAt(ctx, session.ID, 2, UserMessage("gap"))
	require.ErrorIs(t, err, ErrPositionGap)

	got, err := store.GetSession(ctx, session.ID)
	require.NoError(t, err)
	require.Len(t, got.Messages, 1)
	assert.Equal(t, "hello", got.Messages[0].Message.Message.Content)
	assert.Equal(t, MessageKindSubagentEnvelope, got.Messages[0].Message.Kind)
}

func TestSQLiteUniquePositionConstraint(t *testing.T) {
	store := openMemorySQLiteStore(t)
	session := New(WithID("unique-position"))
	require.NoError(t, store.UpdateSession(t.Context(), session))

	_, err := store.AddMessage(t.Context(), session.ID, UserMessage("first"))
	require.NoError(t, err)
	_, err = store.db.ExecContext(t.Context(),
		`INSERT INTO session_items (session_id, position, item_type, message_json) VALUES (?, 0, 'message', '{}')`, session.ID)
	require.Error(t, err)
}

func TestMessageKindPersistsAcrossSQLiteRoundTrip(t *testing.T) {
	store := openMemorySQLiteStore(t)
	session := New(WithID("kind-roundtrip"))
	session.AddMessage(SubagentEnvelopeMessage("envelope"))
	require.NoError(t, store.AddSession(t.Context(), session))

	got, err := store.GetSession(t.Context(), session.ID)
	require.NoError(t, err)
	require.Len(t, got.Messages, 1)
	assert.Equal(t, MessageKindSubagentEnvelope, got.Messages[0].Message.Kind)
	assert.True(t, got.Messages[0].Message.Implicit)
}

func TestEmbeddedSubSessionPersistsParentRootID(t *testing.T) {
	store := openMemorySQLiteStore(t)
	root := New(WithID("embedded-root"))
	child := New(WithID("embedded-child"))
	grandchild := New(WithID("embedded-grandchild"))
	child.AddSubSession(grandchild)
	root.AddSubSession(child)

	require.NoError(t, store.AddSession(t.Context(), root))

	got, err := store.GetSession(t.Context(), grandchild.ID)
	require.NoError(t, err)
	assert.Equal(t, child.ID, got.ParentID)
	assert.Equal(t, root.ID, got.RootID)

	rootID, err := store.ResolveRootID(t.Context(), grandchild.ID)
	require.NoError(t, err)
	assert.Equal(t, root.ID, rootID)
}

func TestRuntimeTopologyMigrationBackfillsNestedTree(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	defer db.Close()

	_, err = db.ExecContext(t.Context(), `
		CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			messages TEXT,
			created_at TEXT,
			parent_id TEXT
		);
		CREATE TABLE session_items (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL,
			position INTEGER NOT NULL,
			item_type TEXT NOT NULL
		);
		INSERT INTO sessions (id, created_at, parent_id) VALUES
			('root', ?, NULL),
			('child', ?, 'root'),
			('grandchild', ?, 'child'),
			('orphan', ?, NULL);
	`, time.Now().Format(time.RFC3339), time.Now().Add(time.Second).Format(time.RFC3339), time.Now().Add(2*time.Second).Format(time.RFC3339), time.Now().Add(3*time.Second).Format(time.RFC3339))
	require.NoError(t, err)

	mgr := NewMigrationManagerWithMigrations(db, []Migration{getAllMigrations()[21]})
	require.NoError(t, mgr.InitializeMigrations(t.Context()))

	rows, err := db.QueryContext(t.Context(), `SELECT id, root_id, runtime_managed FROM sessions ORDER BY id`)
	require.NoError(t, err)
	defer rows.Close()

	got := map[string]struct {
		rootID         string
		runtimeManaged bool
	}{}
	for rows.Next() {
		var id string
		var rootID string
		var runtimeManaged bool
		require.NoError(t, rows.Scan(&id, &rootID, &runtimeManaged))
		got[id] = struct {
			rootID         string
			runtimeManaged bool
		}{rootID: rootID, runtimeManaged: runtimeManaged}
	}
	require.NoError(t, rows.Err())

	for _, id := range []string{"root", "child", "grandchild"} {
		assert.Equal(t, "root", got[id].rootID, "root_id for %s", id)
		assert.False(t, got[id].runtimeManaged, "runtime_managed for %s", id)
	}
	assert.Equal(t, "orphan", got["orphan"].rootID)
	assert.False(t, got["orphan"].runtimeManaged)
}

func TestSessionItemsUniquePositionMigrationFailsOnDuplicates(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	defer db.Close()

	_, err = db.ExecContext(t.Context(), `
		CREATE TABLE session_items (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL,
			position INTEGER NOT NULL,
			item_type TEXT NOT NULL
		);
		CREATE INDEX idx_session_items_session ON session_items(session_id, position);
		INSERT INTO session_items (session_id, position, item_type) VALUES
			('dup', 0, 'message'),
			('dup', 0, 'summary');
	`)
	require.NoError(t, err)

	mgr := NewMigrationManagerWithMigrations(db, []Migration{getAllMigrations()[22]})
	err = mgr.InitializeMigrations(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "UNIQUE constraint failed")
}

func openMemorySQLiteStore(t *testing.T) *SQLiteSessionStore {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	store, err := NewSQLiteSessionStoreFromDB(db)
	if err != nil {
		_ = db.Close()
	}
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return store
}
