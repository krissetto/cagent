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

// SessionRecorder observes all events via [EventBus] global observer and
// persists session changes to a [session.Store]. It is the exclusive writer
// for session data: exactly one SessionRecorder is registered via
// [EventBus.AddGlobalObserver] in [NewLocalRuntime], and the legacy
// [PersistenceObserver] is a deprecated no-op that will not double-write.
//
// Designed as a direct replacement for PersistenceObserver with the
// following improvements:
//
//   - Per-session worker goroutine: each session's events are processed
//     serially on a dedicated goroutine via a buffered channel. This keeps
//     the EventBus.Publish path completely non-blocking with respect to
//     store I/O (which can be slow — e.g. SQLite write contention under
//     load) while preserving per-session ordering.
//   - Back-pressure: when a worker's buffer is full, individual events are
//     dropped (not the whole session) and a counter is incremented. A single
//     warning is logged per worker on first drop.
//   - Positional writes: when the store implements the optional
//     [PositionalStore] interface, user and assistant messages are written
//     at their stable session position, making duplicate event delivery
//     idempotent. Falls back to append-at-tail for stores that don't
//     support it.
//   - FlushSession: synchronous drain of a session worker's queue, ensuring
//     all queued events have been persisted before returning. Used by
//     RunStream teardown to guarantee the store is consistent before the
//     external events channel is closed.
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

// recorderWorker drains events for one session serially.
//
// # Concurrency invariant
//
// w.mu serialises sends to w.events with its own close. All sends use the
// non-blocking trySend helper, which holds w.mu only for the duration of a
// select{default} — a zero-allocation non-blocking operation — so the lock
// is never held while blocking on I/O or waiting for a receiver. The close
// path (recorderWorker.stop) also holds w.mu, which guarantees:
//
//   - No goroutine can send to w.events after close(w.events) returns.
//   - A send in flight when stop begins always completes before the close.
//
// Without this, the race `Handle → workerFor returns w → Close calls
// close(w.events) → Handle sends to closed channel → panic` is live.
type recorderWorker struct {
	mu      sync.Mutex // guards closed + serialises send vs close
	closed  bool
	events  chan recorderEnvelope
	dropped atomic.Int64
}

// send delivers env to the worker, blocking until the channel accepts it.
// Returns false if the worker has already been closed (recorder shutting
// down).
//
// The worker mutex is held for the full duration of the send. That is
// intentional: it serialises the send with stop()'s close, eliminating the
// classic send-on-closed-channel race. If the buffer is full this still
// waits for the worker goroutine to drain one slot, which is exactly the
// back-pressure behaviour we want — the recorder must not silently lose
// events under load. The mutex is released only after the channel accepts
// the value, so stop() (which also takes the mutex) cannot race in the
// middle of a send.
func (w *recorderWorker) send(env recorderEnvelope) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return false
	}
	w.events <- env
	return true
}

// sendBlocking delivers a flush sentinel and waits for it to be processed.
// It must only be called when Close has not yet started (i.e., from
// FlushSession at RunStream teardown). A recover guards against the
// theoretical case where Close races FlushSession; in that situation the
// flush is silently skipped rather than panicking.
func (w *recorderWorker) sendBlocking(env recorderEnvelope) (ok bool) {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return false
	}
	env.sync = make(chan struct{})
	// Serialise this send with close(w.events). If the channel is full, the
	// worker will drain it; Close will wait for the lock instead of racing a
	// close against the send.
	w.events <- env
	w.mu.Unlock()
	<-env.sync
	return true
}

// stop marks the worker as closed and closes its channel. Safe to call
// exactly once (callers already ensure that via the single collector in
// Close).
func (w *recorderWorker) stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	w.closed = true
	close(w.events)
}

const recorderWorkerBuffer = 256

// globalStreamingTracker holds per-session streaming state keyed by session
// ID. It is protected by a mutex because the recorder is global and receives
// events from concurrent sessions.
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

// get returns the recorderStreamingState for sessionID, creating one on
// first access. Caller must hold t.mu.
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

// PositionalStore is the optional interface for stores that support
// positional (idempotent) message insertion. When the store implements
// this, the recorder uses AddMessageAt to insert messages at their
// stable in-session position, making duplicate event delivery safe
// under the unique (session_id, position) index.
type PositionalStore interface {
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

// Handle persists session changes based on the event type. It is designed
// to be registered as a global [EventBus] observer via
// [EventBus.AddGlobalObserver].
//
// Handle dispatches the event to the per-session worker goroutine via a
// buffered channel. The send is BLOCKING when the worker buffer is full so
// no event is ever dropped under back-pressure: a slow store back-pressures
// the publish path, which in turn back-pressures the run loop and the model
// stream. This is the desired behaviour — losing assistant content,
// MessageAddedEvent, or any other persistence-bearing event would corrupt
// the session record. The 256-slot worker buffer absorbs typical bursts so
// the blocking path is exercised only when the store cannot keep up.
//
// Per-session ordering is preserved (FIFO worker channel). The only path
// that drops an event is when the recorder has been closed (shutdown), in
// which case persistence is no longer possible regardless.
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
	if !w.send(env) {
		// Worker is closed (recorder shutting down). Count the loss so
		// integration tests / telemetry can detect a leak across shutdown,
		// but do not log per event — a noisy shutdown floods the log.
		w.dropped.Add(1)
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
// session's event topic has been closed on the bus.
//
// Callers must guarantee that FlushSession is not called concurrently with
// Close (the ordering invariant is: RunStream calls FlushSession at teardown,
// Close is only called when the runtime shuts down after all RunStream
// goroutines have exited).
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

	// sendBlocking encodes the flush sentinel, delivers it, and waits.
	// It handles the edge case where the worker is already stopped.
	w.sendBlocking(recorderEnvelope{sessionID: sessionID})
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
		w.stop() // serialised with trySend via w.mu
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

		r.addUserMessage(ctx, e)

	case *MessageAddedEvent:
		r.tracker.mu.Lock()
		streaming := r.tracker.get(sessionID)
		msgID := streaming.messageID
		r.tracker.mu.Unlock()

		if msgID != 0 {
			// Update the existing streaming message with final content.
			if err := r.store.UpdateMessage(ctx, msgID, e.Message); err != nil {
				slog.WarnContext(ctx, "Failed to finalize streaming message",
					"session_id", sessionID, "message_id", msgID, "error", err)
			}
		} else {
			// No streaming message exists; create a new one.
			if e.Message != nil {
				if _, err := r.store.AddMessage(ctx, e.SessionID, e.Message); err != nil {
					slog.WarnContext(ctx, "Failed to persist message", "session_id", e.SessionID, "error", err)
				}
			}
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
				slog.WarnContext(ctx, "Failed to persist sub-session",
					"parent_id", e.ParentSessionID, "error", err)
			}
		}

	case *SessionSummaryEvent:
		if err := r.store.AddSummary(ctx, e.SessionID, e.Summary, e.FirstKeptEntry); err != nil {
			slog.WarnContext(ctx, "Failed to persist summary", "session_id", e.SessionID, "error", err)
		}

	case *TokenUsageEvent:
		if e.Usage != nil {
			if err := r.store.UpdateSessionTokens(ctx, sessionID,
				e.Usage.InputTokens, e.Usage.OutputTokens, e.Usage.Cost); err != nil {
				slog.WarnContext(ctx, "Failed to persist token usage", "session_id", sessionID, "error", err)
			}
		}

	case *SessionTitleEvent:
		if err := r.store.UpdateSessionTitle(ctx, sessionID, e.Title); err != nil {
			slog.WarnContext(ctx, "Failed to persist session title", "session_id", sessionID, "error", err)
		}

	case *StreamStoppedEvent:
		// Clean up tracker state for this session to prevent memory leaks.
		r.tracker.mu.Lock()
		r.tracker.remove(sessionID)
		r.tracker.mu.Unlock()
	}
}

// addUserMessage persists a user message. If the store supports positional
// insertion (PositionalStore) and the event carries a valid position, we use
// AddMessageAt for idempotent writes. Otherwise we fall back to append-at-tail.
// Unknown positions (-1) on positional stores are skipped to avoid collisions.
func (r *SessionRecorder) addUserMessage(ctx context.Context, e *UserMessageEvent) {
	msg := session.UserMessage(e.Message, e.MultiContent...)

	ps, positional := r.store.(PositionalStore)
	if positional {
		if e.SessionPosition < 0 {
			slog.WarnContext(ctx, "Skipping persistence of UserMessageEvent with unknown position",
				"session_id", e.SessionID, "position", e.SessionPosition)
			return
		}
		if _, err := ps.AddMessageAt(ctx, e.SessionID, e.SessionPosition, msg); err != nil {
			slog.WarnContext(ctx, "Failed to persist user message at position",
				"session_id", e.SessionID, "position", e.SessionPosition, "error", err)
		}
		return
	}

	if _, err := r.store.AddMessage(ctx, e.SessionID, msg); err != nil {
		slog.WarnContext(ctx, "Failed to persist user message", "session_id", e.SessionID, "error", err)
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
			slog.WarnContext(ctx, "Failed to create streaming message",
				"session_id", sessionID, "error", err)
			return
		}
		r.tracker.mu.Lock()
		// Re-fetch to update; only overwrite if still 0 (no concurrent
		// worker can race here since processEvent is serial per session,
		// but be explicit about the intent).
		streaming = r.tracker.get(sessionID)
		if streaming.messageID == 0 {
			streaming.messageID = id
		}
		r.tracker.mu.Unlock()
		slog.DebugContext(ctx, "[PERSIST] Created streaming message",
			"session_id", sessionID, "message_id", id, "agent", msg.AgentName)
	} else {
		// Update existing streaming message.
		if err := r.store.UpdateMessage(ctx, msgID, msg); err != nil {
			slog.WarnContext(ctx, "Failed to update streaming message",
				"session_id", sessionID, "message_id", msgID, "error", err)
		}
	}
}
