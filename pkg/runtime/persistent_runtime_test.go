package runtime

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
)

func TestPersistedUserMessage_DelegationNotificationUsesSubagentResult(t *testing.T) {
	event := &UserMessageEvent{
		SessionID:  "sess-1",
		AgentContext: AgentContext{AgentName: "planner"},
		Message:    "background child completed",
		Kind:       "delegation-notification",
	}

	msg := persistedUserMessage(event)
	require.NotNil(t, msg)
	assert.True(t, msg.IsSubagentResult)
	assert.Equal(t, "planner", msg.AgentName)
	assert.Equal(t, event.Message, msg.Message.Content)
}

func TestPersistChildEvent_PersistsDelegationNotificationAsSubagentResult(t *testing.T) {
	ctx := context.Background()
	store := session.NewInMemorySessionStore()
	sess := session.New()
	require.NoError(t, store.AddSession(ctx, sess))

	persistChildEvent(ctx, store, sess.ID, &UserMessageEvent{
		SessionID: sess.ID,
		AgentContext: AgentContext{AgentName: "worker"},
		Message:   "child finished",
		Kind:      "delegation-notification",
	}, &streamingState{})

	loaded, err := store.GetSession(ctx, sess.ID)
	require.NoError(t, err)
	require.Len(t, loaded.Messages, 1)
	require.NotNil(t, loaded.Messages[0].Message)
	assert.True(t, loaded.Messages[0].Message.IsSubagentResult)
	assert.Equal(t, "worker", loaded.Messages[0].Message.AgentName)
	assert.Equal(t, "child finished", loaded.Messages[0].Message.Message.Content)
}

func TestDelegationNotificationEventCarriesAgentName(t *testing.T) {
	event := DelegationNotification("worker (ab123) has responded", "worker", "sess-1", 4)
	userEvent, ok := event.(*UserMessageEvent)
	require.True(t, ok)
	assert.Equal(t, "delegation-notification", userEvent.Kind)
	assert.Equal(t, "worker", userEvent.AgentName)
	assert.Equal(t, "sess-1", userEvent.SessionID)
	assert.Equal(t, 4, userEvent.SessionPosition)
}

func TestSQLiteSessionStore_PreservesSubagentResultFlag(t *testing.T) {
	ctx := context.Background()
	store, err := session.NewSQLiteSessionStore(filepath.Join(t.TempDir(), "subagent_result.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()

	sess := session.New()
	require.NoError(t, store.AddSession(ctx, sess))

	_, err = store.AddMessage(ctx, sess.ID, session.SubagentResultMessage("worker", "child finished"))
	require.NoError(t, err)

	loaded, err := store.GetSession(ctx, sess.ID)
	require.NoError(t, err)
	require.Len(t, loaded.Messages, 1)
	require.NotNil(t, loaded.Messages[0].Message)
	assert.True(t, loaded.Messages[0].Message.IsSubagentResult)
	assert.Equal(t, "worker", loaded.Messages[0].Message.AgentName)
	assert.Equal(t, "child finished", loaded.Messages[0].Message.Message.Content)
}

func TestSQLiteSessionStore_Migration022_SubagentResultRoundTrip(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "subagent_result_upgrade.db")

	store, err := session.NewSQLiteSessionStore(dbPath)
	require.NoError(t, err)
	require.NoError(t, store.Close())

	// Simulate a pre-022 database by removing the latest migration record and
	// dropping the new column. Re-opening the store should re-apply migration 022.
	sqlDB, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer sqlDB.Close()

	_, err = sqlDB.ExecContext(ctx, `DELETE FROM migrations WHERE id = 22`)
	require.NoError(t, err)
	_, err = sqlDB.ExecContext(ctx, `ALTER TABLE session_items DROP COLUMN is_subagent_result`)
	require.NoError(t, err)

	store, err = session.NewSQLiteSessionStore(dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()

	sess := session.New()
	require.NoError(t, store.AddSession(ctx, sess))
	_, err = store.AddMessage(ctx, sess.ID, session.SubagentResultMessage("planner", "done"))
	require.NoError(t, err)

	loaded, err := store.GetSession(ctx, sess.ID)
	require.NoError(t, err)
	require.Len(t, loaded.Messages, 1)
	require.NotNil(t, loaded.Messages[0].Message)
	assert.True(t, loaded.Messages[0].Message.IsSubagentResult)
	assert.Equal(t, "planner", loaded.Messages[0].Message.AgentName)
	assert.Equal(t, "done", loaded.Messages[0].Message.Message.Content)
}
