package runtime

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
)

// nonPositionalStore wraps a regular session.Store but deliberately does NOT
// expose AddMessageAt, so the recorder treats it as the legacy/append path.
// Required because pkg/session.InMemorySessionStore now implements
// PositionalStore, which would otherwise change the test path.
type nonPositionalStore struct {
	inner session.Store
}

func newNonPositionalStore() *nonPositionalStore {
	return &nonPositionalStore{inner: session.NewInMemorySessionStore()}
}

func (s *nonPositionalStore) AddSession(ctx context.Context, sess *session.Session) error {
	return s.inner.AddSession(ctx, sess)
}

func (s *nonPositionalStore) GetSession(ctx context.Context, id string) (*session.Session, error) {
	return s.inner.GetSession(ctx, id)
}

func (s *nonPositionalStore) GetSessions(ctx context.Context) ([]*session.Session, error) {
	return s.inner.GetSessions(ctx)
}

func (s *nonPositionalStore) GetSessionSummaries(ctx context.Context) ([]session.Summary, error) {
	return s.inner.GetSessionSummaries(ctx)
}

func (s *nonPositionalStore) DeleteSession(ctx context.Context, id string) error {
	return s.inner.DeleteSession(ctx, id)
}

func (s *nonPositionalStore) UpdateSession(ctx context.Context, sess *session.Session) error {
	return s.inner.UpdateSession(ctx, sess)
}

func (s *nonPositionalStore) SetSessionStarred(ctx context.Context, id string, starred bool) error {
	return s.inner.SetSessionStarred(ctx, id, starred)
}

func (s *nonPositionalStore) AddMessage(ctx context.Context, sessionID string, msg *session.Message) (int64, error) {
	return s.inner.AddMessage(ctx, sessionID, msg)
}

func (s *nonPositionalStore) UpdateMessage(ctx context.Context, messageID int64, msg *session.Message) error {
	return s.inner.UpdateMessage(ctx, messageID, msg)
}

func (s *nonPositionalStore) AddSubSession(ctx context.Context, parentID string, sub *session.Session) error {
	return s.inner.AddSubSession(ctx, parentID, sub)
}

func (s *nonPositionalStore) AddSummary(ctx context.Context, sessionID, summary string, firstKept int) error {
	return s.inner.AddSummary(ctx, sessionID, summary, firstKept)
}

func (s *nonPositionalStore) UpdateSessionTokens(ctx context.Context, sessionID string, in, out int64, cost float64) error {
	return s.inner.UpdateSessionTokens(ctx, sessionID, in, out, cost)
}

func (s *nonPositionalStore) UpdateSessionTitle(ctx context.Context, sessionID, title string) error {
	return s.inner.UpdateSessionTitle(ctx, sessionID, title)
}

func (s *nonPositionalStore) Close() error {
	return s.inner.Close()
}

// positionalTestStore wraps a regular session.Store and adds a test-only
// PositionalStore implementation with duplicate suppression by
// (session_id, position). It does not attempt true positional insertion in
// the underlying store — the recorder tests only need idempotent behaviour.
type positionalTestStore struct {
	session.Store

	mu        sync.Mutex
	positions map[string]map[int]int64
}

func newPositionalTestStore() *positionalTestStore {
	return &positionalTestStore{
		Store:     session.NewInMemorySessionStore(),
		positions: make(map[string]map[int]int64),
	}
}

func (s *positionalTestStore) AddMessageAt(ctx context.Context, sessionID string, position int, msg *session.Message) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	bySession, ok := s.positions[sessionID]
	if !ok {
		bySession = make(map[int]int64)
		s.positions[sessionID] = bySession
	}
	if id, exists := bySession[position]; exists {
		return id, nil // idempotent duplicate delivery
	}
	id, err := s.AddMessage(ctx, sessionID, msg)
	if err != nil {
		return 0, err
	}
	bySession[position] = id
	return id, nil
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

// TestSessionRecorder_UserMessageFallbackAppend verifies the recorder works
// with a plain store that does not implement PositionalStore.
func TestSessionRecorder_UserMessageFallbackAppend(t *testing.T) {
	store := newNonPositionalStore()
	sess := session.New()
	require.NoError(t, store.UpdateSession(t.Context(), sess))

	rec := NewSessionRecorder(store)
	t.Cleanup(func() { rec.Close() })

	rec.Handle(sess.ID, UserMessage("hello", sess.ID, nil))
	rec.FlushSession(sess.ID)

	got, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Len(t, got.Messages, 1)
	assert.Equal(t, chat.MessageRoleUser, got.Messages[0].Message.Message.Role)
	assert.Equal(t, "hello", got.Messages[0].Message.Message.Content)
}

// TestSessionRecorder_DuplicateUserMessageAtPositionIsIdempotent verifies
// that when the store supports PositionalStore, delivering the same
// UserMessageEvent twice does not duplicate the row.
func TestSessionRecorder_DuplicateUserMessageAtPositionIsIdempotent(t *testing.T) {
	store := newPositionalTestStore()
	sess := session.New()
	require.NoError(t, store.UpdateSession(t.Context(), sess))

	rec := NewSessionRecorder(store)
	t.Cleanup(func() { rec.Close() })

	ev := UserMessage("hello", sess.ID, nil, 0)
	rec.Handle(sess.ID, ev)
	rec.Handle(sess.ID, ev)
	rec.FlushSession(sess.ID)

	got, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Len(t, got.Messages, 1,
		"duplicate UserMessageEvent must not produce a second row when positional writes are available")
	assert.Equal(t, "hello", got.Messages[0].Message.Message.Content)
}

// TestSessionRecorder_UnknownUserPositionSkippedOnPositionalStore verifies
// that a positional store does not append a user message whose position is
// unknown, avoiding collisions when AddMessageAt becomes backed by a unique
// (session_id, position) index.
func TestSessionRecorder_UnknownUserPositionSkippedOnPositionalStore(t *testing.T) {
	store := newPositionalTestStore()
	sess := session.New()
	require.NoError(t, store.UpdateSession(t.Context(), sess))

	rec := NewSessionRecorder(store)
	t.Cleanup(func() { rec.Close() })

	rec.Handle(sess.ID, UserMessage("hello", sess.ID, nil)) // no explicit position => -1
	rec.FlushSession(sess.ID)

	got, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	assert.Empty(t, got.Messages,
		"unknown-position user events must be skipped on positional stores")
}

// TestSessionRecorder_StreamingRowConsistency verifies the recorder keeps a
// single in-flight streaming row and finalizes it with the MessageAddedEvent
// payload rather than creating a second assistant row.
func TestSessionRecorder_StreamingRowConsistency(t *testing.T) {
	store := session.NewInMemorySessionStore()
	sess := session.New()
	require.NoError(t, store.UpdateSession(t.Context(), sess))

	rec := NewSessionRecorder(store)
	t.Cleanup(func() { rec.Close() })

	rec.Handle(sess.ID, AgentChoice("agent", sess.ID, "hel"))
	rec.Handle(sess.ID, AgentChoiceReasoning("agent", sess.ID, "thinking"))
	rec.Handle(sess.ID, AgentChoice("agent", sess.ID, "lo"))

	final := &session.Message{
		AgentName: "agent",
		Message: chat.Message{
			Role:             chat.MessageRoleAssistant,
			Content:          "hello",
			ReasoningContent: "thinking",
		},
	}
	rec.Handle(sess.ID, MessageAdded(sess.ID, final, "agent"))
	rec.FlushSession(sess.ID)

	got, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Len(t, got.Messages, 1,
		"streaming chunks plus final MessageAddedEvent must result in one assistant row")
	require.NotNil(t, got.Messages[0].Message)
	assert.Equal(t, chat.MessageRoleAssistant, got.Messages[0].Message.Message.Role)
	assert.Equal(t, "hello", got.Messages[0].Message.Message.Content)
	assert.Equal(t, "thinking", got.Messages[0].Message.Message.ReasoningContent)
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

	rec.Handle(sess.ID, UserMessage("hello", sess.ID, nil, 0))
	rec.FlushSession(sess.ID)

	got, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Len(t, got.Messages, 1,
		"FlushSession must block until all prior events are persisted")
	assert.Equal(t, "hello", got.Messages[0].Message.Message.Content)
}

// TestSessionRecorder_AsyncHandle_NonBlocking verifies that Handle returns
// quickly even when the underlying store is slow, demonstrating that the
// EventBus.Publish path is not blocked by store I/O.
// TestSessionRecorder_HandleBlocksOnFullBufferAndPersistsAll verifies the
// robustness contract of the recorder: when the per-session worker buffer
// fills up because the store is slow, Handle MUST back-pressure the caller
// rather than drop events. Every event handed to the recorder must end up
// persisted (or queued for persistence) by the time Close returns.
//
// This is the inverse of an earlier non-blocking contract: silently dropping
// AgentChoice / AgentChoiceReasoning chunks under load was acceptable on the
// theory that the final MessageAddedEvent would carry the full content. That
// theory broke down in practice because MessageAddedEvent itself could land
// in a full buffer and be dropped, corrupting the persisted session.
func TestSessionRecorder_HandleBlocksOnFullBufferAndPersistsAll(t *testing.T) {
	t.Parallel()

	store := session.NewInMemorySessionStore()
	sess := session.New()
	require.NoError(t, store.UpdateSession(t.Context(), sess))

	slow := &slowStore{Store: store, delay: 5 * time.Millisecond}
	rec := NewSessionRecorder(slow)

	// eventCount is intentionally larger than the worker buffer
	// (recorderWorkerBuffer = 256) so the blocking path is exercised.
	const eventCount = recorderWorkerBuffer * 4

	for range eventCount {
		rec.Handle(sess.ID, AgentChoice("agent", sess.ID, "x"))
	}

	rec.workersMu.Lock()
	worker := rec.workers[sess.ID]
	rec.workersMu.Unlock()
	require.NotNil(t, worker, "expected recorder to create a per-session worker")

	// Close drains the worker so all queued events are persisted.
	rec.Close()

	assert.Equal(t, int64(0), worker.dropped.Load(),
		"recorder must not drop any events when the worker buffer fills up; "+
			"a slow store should back-pressure the caller, not silently lose data")
	assert.Equal(t, int64(eventCount), slow.totalWrites.Load(),
		"every event must reach the store under sustained back-pressure")
}

// TestSessionRecorder_UsesScopedSessionID verifies that if an event carries a
// more specific SessionScoped session id than the bus topic, the recorder
// routes persistence to the scoped session.
func TestSessionRecorder_UsesScopedSessionID(t *testing.T) {
	store := session.NewInMemorySessionStore()
	parent := session.New(session.WithID("parent"))
	child := session.New(session.WithID("child"))
	require.NoError(t, store.UpdateSession(t.Context(), parent))
	require.NoError(t, store.UpdateSession(t.Context(), child))

	rec := NewSessionRecorder(store)
	t.Cleanup(func() { rec.Close() })

	// Deliver on the parent topic but tag the event to child via SessionScoped.
	rec.Handle(parent.ID, UserMessage("hello child", child.ID, nil, 0))
	rec.FlushSession(child.ID)

	loadedParent, err := store.GetSession(t.Context(), parent.ID)
	require.NoError(t, err)
	loadedChild, err := store.GetSession(t.Context(), child.ID)
	require.NoError(t, err)

	assert.Empty(t, loadedParent.Messages)
	require.Len(t, loadedChild.Messages, 1)
	assert.Equal(t, "hello child", loadedChild.Messages[0].Message.Message.Content)
}

// TestSessionRecorder_HandleDoesNotRaceClose is a regression test for the
// send-vs-close window on worker.events. Historically, Handle could obtain a
// worker pointer, then Close could close the worker channel before Handle
// executed the send, producing a send-on-closed-channel panic (and a race
// report under -race). The worker-local send/close mutex in recorder.go
// serialises these operations.
func TestSessionRecorder_HandleDoesNotRaceClose(t *testing.T) {
	for range 50 {
		store := session.NewInMemorySessionStore()
		sess := session.New()
		require.NoError(t, store.UpdateSession(t.Context(), sess))

		rec := NewSessionRecorder(store)

		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)

		go func() {
			defer wg.Done()
			<-start
			for range 100 {
				assert.NotPanics(t, func() {
					rec.Handle(sess.ID, UserMessage("hello", sess.ID, nil))
				})
			}
		}()

		go func() {
			defer wg.Done()
			<-start
			assert.NotPanics(t, func() {
				rec.Close()
			})
		}()

		close(start)
		wg.Wait()
	}
}
