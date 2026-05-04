package runtime

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
)

// SessionRecorder observes all events via EventBus global observer and
// persists session changes to a [session.Store]. It replaces the
// per-session persistence logic that previously lived in separate
// event-handling and stream-wrapping code paths.
//
// The recorder is goroutine-safe and asynchronous: each session has its
// own buffered worker goroutine that drains events serially and performs
// the actual store I/O. This keeps the EventBus.Publish path off any
// blocking [session.Store] call (which can be slow under load — e.g.
// SQLite write contention) while preserving per-session ordering.
//
// Back-pressure: when a worker's buffer is full, individual events are
// dropped (not the whole session) and the dropped count is incremented.
// A single warning is logged per worker on first drop to avoid log spam.
type SessionRecorder struct {
	store   session.Store
	tracker *globalStreamingTracker

	workersMu sync.Mutex
	workers   map[string]*recorderWorker
	wg        sync.WaitGroup
	closed    atomic.Bool
}

// recorderEnvelope is the unit of work shipped from the publish path to a
// per-session worker goroutine. When sync is non-nil the envelope is a
// synchronisation sentinel — the worker closes sync rather than processing
// an event, allowing the caller to block until all prior events have been
// persisted.
type recorderEnvelope struct {
	sessionID string
	ev        Event
	sync      chan struct{} // non-nil for flush sentinels; nil for normal events
}

// recorderWorker drains events for one session serially. Its buffered
// channel preserves ordering; a full buffer drops the event rather than
// blocking the caller.
type recorderWorker struct {
	events     chan recorderEnvelope
	dropped    atomic.Int64
	loggedDrop atomic.Bool
}

const recorderWorkerBuffer = 256

// globalStreamingTracker holds per-session streaming state keyed by session ID.
// It is protected by a mutex because the recorder is global and receives events
// from concurrent sessions.
type globalStreamingTracker struct {
	mu     sync.Mutex
	states map[string]*recorderStreamingState
}

// recorderStreamingState tracks the accumulated content for a streaming
// assistant message within a single session.
type recorderStreamingState struct {
	content          strings.Builder
	reasoningContent strings.Builder
	agentName        string
	messageID        int64 // ID of the current streaming message (0 if none)
}

// get returns the recorderStreamingState for sessionID, creating one on first access.
// Caller must hold t.mu.
func (t *globalStreamingTracker) get(sessionID string) *recorderStreamingState {
	s, ok := t.states[sessionID]
	if !ok {
		s = &recorderStreamingState{}
		t.states[sessionID] = s
	}
	return s
}

// remove deletes the streaming state for sessionID to prevent memory leaks.
// Caller must hold t.mu.
func (t *globalStreamingTracker) remove(sessionID string) {
	delete(t.states, sessionID)
}

// positionalStore is the optional per-session-position insertion capability.
// When the underlying session.Store implements this interface, the recorder
// inserts messages at their stable in-session positions, making duplicate
// event delivery idempotent under the unique (session_id, position) index.
type positionalStore interface {
	AddMessageAt(ctx context.Context, sessionID string, position int, msg *session.Message) (int64, error)
}

// NewSessionRecorder creates a SessionRecorder backed by the given store.
func NewSessionRecorder(store session.Store) *SessionRecorder {
	return &SessionRecorder{
		store: store,
		tracker: &globalStreamingTracker{
			states: make(map[string]*recorderStreamingState),
		},
		workers: make(map[string]*recorderWorker),
	}
}

// Handle persists session changes based on the event type. It is intended to
// be registered as a global EventBus observer.
//
// Handle is non-blocking: it dispatches the event to the per-session worker
// goroutine via a buffered channel. If the worker's buffer is full the event
// is dropped (a per-worker counter is incremented and a single warning is
// logged on first drop). Per-session ordering is preserved.
func (r *SessionRecorder) Handle(sessionID string, ev Event) {
	if sessionID == "" || ev == nil {
		return
	}

	// Resolve effective session ID from SessionScoped when available; this
	// is how child-session events forwarded through a parent's topic carry
	// their own session identity.
	if scoped, ok := ev.(SessionScoped); ok {
		if sid := scoped.GetSessionID(); sid != "" {
			sessionID = sid
		}
	}

	if r.closed.Load() {
		return
	}

	w := r.workerFor(sessionID)
	if w == nil {
		return
	}
	env := recorderEnvelope{sessionID: sessionID, ev: ev}
	select {
	case w.events <- env:
	default:
		w.dropped.Add(1)
		if w.loggedDrop.CompareAndSwap(false, true) {
			slog.Warn("SessionRecorder dropping events: per-session worker queue is full",
				"session_id", sessionID, "buffer", recorderWorkerBuffer)
		}
	}
}

// workerFor returns the worker responsible for sessionID, lazily creating
// one on first access.
func (r *SessionRecorder) workerFor(sessionID string) *recorderWorker {
	r.workersMu.Lock()
	defer r.workersMu.Unlock()
	if r.closed.Load() {
		return nil
	}
	if w, ok := r.workers[sessionID]; ok {
		return w
	}
	w := &recorderWorker{
		events: make(chan recorderEnvelope, recorderWorkerBuffer),
	}
	r.workers[sessionID] = w
	r.wg.Add(1)
	go r.runWorker(w)
	return w
}

// runWorker drains a worker's events channel until it is closed.
func (r *SessionRecorder) runWorker(w *recorderWorker) {
	defer r.wg.Done()
	for env := range w.events {
		if env.sync != nil {
			close(env.sync)
			continue
		}
		r.processEvent(env.sessionID, env.ev)
	}
}

// FlushSession blocks until all events currently queued for sessionID have
// been persisted by that session's worker. It is safe to call after the
// session's event topic has been closed.
func (r *SessionRecorder) FlushSession(sessionID string) {
	if sessionID == "" || r.closed.Load() {
		return
	}

	r.workersMu.Lock()
	w, ok := r.workers[sessionID]
	r.workersMu.Unlock()
	if !ok || w == nil {
		return
	}

	syncCh := make(chan struct{})
	env := recorderEnvelope{sessionID: sessionID, sync: syncCh}

	// The recorder only flushes live sessions at stream teardown, before
	// recorder.Close() begins shutting workers down. The worker channel is
	// buffered and should always have room for this sentinel once the stream's
	// publisher goroutine has stopped enqueueing new events for the session.
	w.events <- env
	<-syncCh
}

// Close stops every worker and waits for them to drain. Safe to call once;
// subsequent calls are no-ops.
func (r *SessionRecorder) Close() {
	if !r.closed.CompareAndSwap(false, true) {
		return
	}
	r.workersMu.Lock()
	workers := make([]*recorderWorker, 0, len(r.workers))
	for _, w := range r.workers {
		workers = append(workers, w)
	}
	// Clear the map so any racing workerFor() call after this point will
	// see closed==true and return nil rather than reviving a worker.
	r.workers = nil
	r.workersMu.Unlock()
	for _, w := range workers {
		close(w.events)
	}
	r.wg.Wait()
}

// processEvent contains the per-event persistence logic. It runs on the
// per-session worker goroutine, so its store calls do not block the
// EventBus.Publish path.
func (r *SessionRecorder) processEvent(sessionID string, ev Event) {
	ctx := context.Background()

	switch e := ev.(type) {
	case *AgentChoiceEvent:
		r.tracker.mu.Lock()
		streaming := r.tracker.get(sessionID)
		streaming.content.WriteString(e.Content)
		streaming.agentName = e.AgentName
		r.tracker.mu.Unlock()

		r.persistStreamingContent(ctx, sessionID)

	case *AgentChoiceReasoningEvent:
		r.tracker.mu.Lock()
		streaming := r.tracker.get(sessionID)
		streaming.reasoningContent.WriteString(e.Content)
		streaming.agentName = e.AgentName
		r.tracker.mu.Unlock()

		r.persistStreamingContent(ctx, sessionID)

	case *UserMessageEvent:
		// Reset streaming state when a user message is received.
		r.tracker.mu.Lock()
		streaming := r.tracker.get(sessionID)
		streaming.content.Reset()
		streaming.reasoningContent.Reset()
		streaming.agentName = ""
		streaming.messageID = 0
		r.tracker.mu.Unlock()

		// Skip persistence for user events with unknown position to avoid
		// unique-constraint collisions where position -1 would collide across
		// messages in the same session.
		if e.SessionPosition < 0 {
			slog.Warn("Skipping persistence of UserMessageEvent with unknown position",
				"session_id", e.SessionID, "position", e.SessionPosition)
			return
		}

		r.addUserMessage(ctx, e)

	case *MessageAddedEvent:
		r.tracker.mu.Lock()
		streaming := r.tracker.get(sessionID)
		msgID := streaming.messageID
		r.tracker.mu.Unlock()

		if msgID != 0 {
			// Update the existing streaming message with final content.
			if err := r.store.UpdateMessage(ctx, msgID, e.Message); err != nil {
				slog.Warn("Failed to finalize streaming message", "session_id", sessionID, "message_id", msgID, "error", err)
			}
		} else {
			// No streaming message exists, create a new one.
			r.addAssistantMessage(ctx, e)
		}

		// Reset streaming state after message is finalized.
		r.tracker.mu.Lock()
		streaming = r.tracker.get(sessionID)
		streaming.content.Reset()
		streaming.reasoningContent.Reset()
		streaming.agentName = ""
		streaming.messageID = 0
		r.tracker.mu.Unlock()

	case *SubSessionCompletedEvent:
		if subSess, ok := e.SubSession.(*session.Session); ok {
			if err := r.store.AddSubSession(ctx, e.ParentSessionID, subSess); err != nil {
				slog.Warn("Failed to persist sub-session", "parent_id", e.ParentSessionID, "error", err)
			}
		}

	case *SessionSummaryEvent:
		if err := r.store.AddSummary(ctx, e.SessionID, e.Summary, e.FirstKeptEntry); err != nil {
			slog.Warn("Failed to persist summary", "session_id", e.SessionID, "error", err)
		}

	case *TokenUsageEvent:
		if e.Usage != nil {
			if err := r.store.UpdateSessionTokens(ctx, sessionID, e.Usage.InputTokens, e.Usage.OutputTokens, e.Usage.Cost); err != nil {
				slog.Warn("Failed to persist token usage", "session_id", sessionID, "error", err)
			}
		}

	case *SessionTitleEvent:
		if err := r.store.UpdateSessionTitle(ctx, sessionID, e.Title); err != nil {
			slog.Warn("Failed to persist session title", "session_id", sessionID, "error", err)
		}

	case *StreamStoppedEvent:
		// Clean up tracker state for this session to prevent memory leaks.
		r.tracker.mu.Lock()
		r.tracker.remove(sessionID)
		r.tracker.mu.Unlock()
	}
}

// addUserMessage persists a user message. If the store supports positional
// insertion, the stable SessionPosition is used so duplicate delivery becomes
// idempotent. Falls back to append-at-tail otherwise.
func (r *SessionRecorder) addUserMessage(ctx context.Context, e *UserMessageEvent) {
	msg := session.UserMessage(e.Message, e.MultiContent...)
	if ps, ok := r.store.(positionalStore); ok && e.SessionPosition >= 0 {
		if _, err := ps.AddMessageAt(ctx, e.SessionID, e.SessionPosition, msg); err != nil {
			slog.Warn("Failed to persist user message at position", "session_id", e.SessionID, "position", e.SessionPosition, "error", err)
		}
		return
	}
	if _, err := r.store.AddMessage(ctx, e.SessionID, msg); err != nil {
		slog.Warn("Failed to persist user message", "session_id", e.SessionID, "error", err)
	}
}

// addAssistantMessage persists an assistant/tool message. If the store
// supports positional insertion and the event carries a valid position, the
// stable position is used for idempotent delivery.
func (r *SessionRecorder) addAssistantMessage(ctx context.Context, e *MessageAddedEvent) {
	if ps, ok := r.store.(positionalStore); ok && e.SessionPosition >= 0 {
		if _, err := ps.AddMessageAt(ctx, e.SessionID, e.SessionPosition, e.Message); err != nil {
			slog.Warn("Failed to persist message at position", "session_id", e.SessionID, "position", e.SessionPosition, "error", err)
		}
		return
	}
	if _, err := r.store.AddMessage(ctx, e.SessionID, e.Message); err != nil {
		slog.Warn("Failed to persist message", "session_id", e.SessionID, "error", err)
	}
}

// persistStreamingContent creates or updates the streaming assistant message.
func (r *SessionRecorder) persistStreamingContent(ctx context.Context, sessionID string) {
	r.tracker.mu.Lock()
	streaming := r.tracker.get(sessionID)

	msg := &session.Message{
		AgentName: streaming.agentName,
		Message: chat.Message{
			Role:             chat.MessageRoleAssistant,
			Content:          streaming.content.String(),
			ReasoningContent: streaming.reasoningContent.String(),
		},
	}
	msgID := streaming.messageID
	r.tracker.mu.Unlock()

	if msgID == 0 {
		// Create new streaming message.
		id, err := r.store.AddMessage(ctx, sessionID, msg)
		if err != nil {
			slog.Warn("Failed to create streaming message", "session_id", sessionID, "error", err)
			return
		}
		r.tracker.mu.Lock()
		// Re-fetch in case another goroutine raced; only update if still 0.
		streaming = r.tracker.get(sessionID)
		if streaming.messageID == 0 {
			streaming.messageID = id
		}
		r.tracker.mu.Unlock()
		slog.Debug("[PERSIST] Created streaming message", "session_id", sessionID, "message_id", id, "agent", msg.AgentName)
	} else {
		// Update existing streaming message.
		if err := r.store.UpdateMessage(ctx, msgID, msg); err != nil {
			slog.Warn("Failed to update streaming message", "session_id", sessionID, "message_id", msgID, "error", err)
		}
	}
}
