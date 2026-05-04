package runtime

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

// TestSessionRecorder_DuplicateUserMessageIsIdempotent verifies that
// delivering the same UserMessageEvent twice does not duplicate the row in
// the SQLite store, thanks to the unique (session_id, position) index plus
// AddMessageAt's INSERT OR IGNORE semantics.
func TestSessionRecorder_DuplicateUserMessageIsIdempotent(t *testing.T) {
	tempDB := filepath.Join(t.TempDir(), "recorder_dup.db")
	store, err := session.NewSQLiteSessionStore(tempDB)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	sess := session.New()
	require.NoError(t, store.UpdateSession(t.Context(), sess))

	rec := NewSessionRecorder(store)
	t.Cleanup(func() { rec.Close() })

	ev := UserMessage("hello", sess.ID, nil, 0)
	rec.Handle(sess.ID, ev)
	rec.Handle(sess.ID, ev) // duplicate delivery

	// Drain the worker so all writes are complete before we read.
	rec.Close()

	got, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Len(t, got.Messages, 1, "duplicate UserMessageEvent must not produce a second row")
	assert.Equal(t, "hello", got.Messages[0].Message.Message.Content)
}

// TestSessionRecorder_NegativeUserPositionIsSkipped verifies that user
// events with SessionPosition < 0 are not persisted (would otherwise
// collide on the unique index for any subsequent insert).
func TestSessionRecorder_NegativeUserPositionIsSkipped(t *testing.T) {
	store := session.NewInMemorySessionStore()
	sess := session.New()
	require.NoError(t, store.UpdateSession(t.Context(), sess))

	rec := NewSessionRecorder(store)

	ev := UserMessage("hello", sess.ID, nil) // no position passed → -1
	rec.Handle(sess.ID, ev)

	// Drain the worker so all writes are complete before we read.
	rec.Close()

	got, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	assert.Empty(t, got.Messages, "events with unknown position must not be persisted")
}

// TestSessionRecorder_AssistantMessageAtPositionIsIdempotent verifies that
// delivering the same MessageAddedEvent with the same SessionPosition twice
// does not duplicate the row.
func TestSessionRecorder_AssistantMessageAtPositionIsIdempotent(t *testing.T) {
	tempDB := filepath.Join(t.TempDir(), "recorder_dup_assist.db")
	store, err := session.NewSQLiteSessionStore(tempDB)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	sess := session.New()
	require.NoError(t, store.UpdateSession(t.Context(), sess))

	rec := NewSessionRecorder(store)

	// First seed a user message at position 0 so the assistant ends up at 1.
	rec.Handle(sess.ID, UserMessage("hi", sess.ID, nil, 0))

	msg := &session.Message{
		AgentName: "root",
		Message: chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: "world",
		},
	}
	ev := MessageAdded(sess.ID, 1, msg, "root")
	rec.Handle(sess.ID, ev)
	rec.Handle(sess.ID, ev) // duplicate delivery

	// Drain the worker so all writes are complete before we read.
	rec.Close()

	got, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Len(t, got.Messages, 2, "duplicate MessageAddedEvent must not produce a second row")
	assert.Equal(t, "world", got.Messages[1].Message.Message.Content)
}

// slowStore wraps a Store and adds a configurable per-write sleep to
// simulate a slow database for async backpressure tests.
type slowStore struct {
	session.Store
	delay       time.Duration
	totalWrites atomic.Int64
}

func (s *slowStore) AddMessage(ctx context.Context, sessionID string, msg *session.Message) (int64, error) {
	s.totalWrites.Add(1)
	time.Sleep(s.delay)
	return s.Store.AddMessage(ctx, sessionID, msg)
}

func (s *slowStore) UpdateMessage(ctx context.Context, messageID int64, msg *session.Message) error {
	s.totalWrites.Add(1)
	time.Sleep(s.delay)
	return s.Store.UpdateMessage(ctx, messageID, msg)
}

// TestSessionRecorder_AsyncHandle_NonBlocking verifies that Handle() returns
// quickly even when the underlying store is slow, demonstrating that the
// EventBus.Publish path is not blocked by store I/O.
func TestSessionRecorder_AsyncHandle_NonBlocking(t *testing.T) {
	t.Parallel()

	store := session.NewInMemorySessionStore()
	sess := session.New()
	require.NoError(t, store.UpdateSession(t.Context(), sess))

	slow := &slowStore{Store: store, delay: 50 * time.Millisecond}
	rec := NewSessionRecorder(slow)

	const eventCount = 100

	start := time.Now()
	for i := 0; i < eventCount; i++ {
		rec.Handle(sess.ID, AgentChoice("agent", sess.ID, "x"))
	}
	elapsed := time.Since(start)

	// Handle() should be non-blocking: 100 calls must complete in well under
	// (100 * 50ms = 5 s). We allow a generous 500 ms to avoid flakiness.
	assert.Less(t, elapsed, 500*time.Millisecond,
		"Handle() must not block the caller for slow store I/O (elapsed: %v)", elapsed)

	rec.workersMu.Lock()
	worker := rec.workers[sess.ID]
	rec.workersMu.Unlock()
	require.NotNil(t, worker, "expected recorder to create a per-session worker")

	// Close drains the worker so all queued events are either persisted or
	// accounted for as dropped.
	rec.Close()

	processedOrDropped := slow.totalWrites.Load() + worker.dropped.Load()
	assert.Equal(t, int64(eventCount), processedOrDropped,
		"every event must be either written or dropped before Close() returns")
}

// TestSessionRecorder_FlushSessionGuaranteesPersistence is a regression test
// for the intermittent TestACPSessionPersistence failure: RunStream's external
// channel closing does NOT mean all recorder workers have flushed to the store.
// After the fix, wrapEventsForObservers calls FlushSession before closing the
// external channel, so this test exercises the full path end-to-end.
func TestSessionRecorder_FlushSessionGuaranteesPersistence(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "flush.db")
	store, err := session.NewSQLiteSessionStore(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	stream := newStreamBuilder().AddContent("hello from agent").AddStopWithUsage(10, 5).Build()
	prov := &mockProvider{id: "test/flush", stream: stream}
	root := agent.New("root", "root", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))

	// Use New() (not NewLocalRuntime) to get the recorder wired as a global
	// event-bus observer — the same setup used by the ACP and API servers.
	rt, err := New(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}), WithSessionStore(store))
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close() })

	sess := session.New(session.WithUserMessage("hi"))
	require.NoError(t, store.UpdateSession(t.Context(), sess))

	// Run the stream to completion.
	for range rt.RunStream(t.Context(), sess) {
	}

	// Immediately read from the store — the assistant message must already
	// be persisted because wrapEventsForObservers flushes before closing.
	loaded, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)

	var hasAssistant bool
	for _, item := range loaded.Messages {
		if item.Message != nil && item.Message.Message.Role == chat.MessageRoleAssistant {
			hasAssistant = true
			assert.Contains(t, item.Message.Message.Content, "hello from agent")
		}
	}
	assert.True(t, hasAssistant,
		"assistant message must be persisted to the store by the time RunStream's external channel closes")
}

// TestSessionRecorder_FlushSessionDrainsSentinel verifies the FlushSession
// implementation detail: a flush sentinel sent through the worker is waited
// on, blocking the caller until all prior events have been processed.
func TestSessionRecorder_FlushSessionDrainsSentinel(t *testing.T) {
	store := session.NewInMemorySessionStore()
	sess := session.New()
	require.NoError(t, store.UpdateSession(t.Context(), sess))

	rec := NewSessionRecorder(store)
	t.Cleanup(func() { rec.Close() })

	// Queue a user message then flush.
	rec.Handle(sess.ID, UserMessage("hello", sess.ID, nil, 0))
	rec.FlushSession(sess.ID)

	got, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Len(t, got.Messages, 1, "FlushSession must block until all prior events are persisted")
	assert.Equal(t, "hello", got.Messages[0].Message.Message.Content)
}
