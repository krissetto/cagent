package runtime

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
)

// SessionRecorder observes all events via the EventBus global observer and is
// the sole writer for runtime session transcript mutations.
type SessionRecorder struct {
	store   session.Store
	tracker *globalStreamingTracker

	workersMu sync.Mutex
	workers   map[string]*recorderWorker
	wg        sync.WaitGroup
	closed    atomic.Bool
}

type recorderEnvelope struct {
	sessionID string
	ev        Event
	sync      chan struct{}
}

type recorderWorker struct {
	mu     sync.Mutex
	closed bool
	events chan recorderEnvelope
}

func (w *recorderWorker) send(env recorderEnvelope) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return false
	}
	w.events <- env
	return true
}

func (w *recorderWorker) sendBlocking(env recorderEnvelope) bool {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return false
	}
	env.sync = make(chan struct{})
	w.events <- env
	w.mu.Unlock()
	<-env.sync
	return true
}

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

type globalStreamingTracker struct {
	mu     sync.Mutex
	states map[string]*recorderStreamingState
}

type recorderStreamingState struct {
	content          strings.Builder
	reasoningContent strings.Builder
	agentName        string
	messageID        int64
}

func (t *globalStreamingTracker) get(sessionID string) *recorderStreamingState {
	s, ok := t.states[sessionID]
	if !ok {
		s = &recorderStreamingState{}
		t.states[sessionID] = s
	}
	return s
}

func (t *globalStreamingTracker) remove(sessionID string) {
	delete(t.states, sessionID)
}

func NewSessionRecorder(store session.Store) *SessionRecorder {
	if store == nil {
		return nil
	}
	return &SessionRecorder{
		store: store,
		tracker: &globalStreamingTracker{
			states: make(map[string]*recorderStreamingState),
		},
		workers: make(map[string]*recorderWorker),
	}
}

func (r *SessionRecorder) Handle(sessionID string, ev Event) {
	if r == nil || sessionID == "" || ev == nil || r.closed.Load() {
		return
	}
	if scoped, ok := ev.(SessionScoped); ok {
		if sid := scoped.GetSessionID(); sid != "" {
			sessionID = sid
		}
	}
	w := r.workerFor(sessionID)
	if w == nil {
		return
	}
	w.send(recorderEnvelope{sessionID: sessionID, ev: ev})
}

func (r *SessionRecorder) workerFor(sessionID string) *recorderWorker {
	r.workersMu.Lock()
	defer r.workersMu.Unlock()
	if r.closed.Load() {
		return nil
	}
	if w, ok := r.workers[sessionID]; ok {
		return w
	}
	w := &recorderWorker{events: make(chan recorderEnvelope, recorderWorkerBuffer)}
	r.workers[sessionID] = w
	r.wg.Add(1)
	go r.runWorker(w)
	return w
}

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

func (r *SessionRecorder) FlushSession(sessionID string) {
	if r == nil || sessionID == "" || r.closed.Load() {
		return
	}
	r.workersMu.Lock()
	w := r.workers[sessionID]
	r.workersMu.Unlock()
	if w == nil {
		return
	}
	w.sendBlocking(recorderEnvelope{sessionID: sessionID})
}

func (r *SessionRecorder) Close() {
	if r == nil || !r.closed.CompareAndSwap(false, true) {
		return
	}
	r.workersMu.Lock()
	workers := make([]*recorderWorker, 0, len(r.workers))
	for _, w := range r.workers {
		workers = append(workers, w)
	}
	r.workers = nil
	r.workersMu.Unlock()
	for _, w := range workers {
		w.stop()
	}
	r.wg.Wait()
}

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
			if err := r.store.UpdateMessage(ctx, msgID, e.Message); err != nil {
				slog.WarnContext(ctx, "Failed to finalize streaming message", "session_id", sessionID, "message_id", msgID, "error", err)
			}
		} else if e.Message != nil {
			if e.SessionPosition >= 0 {
				if ps, ok := r.store.(session.PositionalStore); ok {
					if _, err := ps.AddMessageAt(ctx, e.SessionID, e.SessionPosition, e.Message); err != nil {
						slog.WarnContext(ctx, "Failed to persist message at position", "session_id", e.SessionID, "position", e.SessionPosition, "error", err)
					}
					break
				}
			}
			if _, err := r.store.AddMessage(ctx, e.SessionID, e.Message); err != nil {
				slog.WarnContext(ctx, "Failed to persist message", "session_id", e.SessionID, "error", err)
			}
		}
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
				slog.WarnContext(ctx, "Failed to persist sub-session", "parent_id", e.ParentSessionID, "error", err)
			}
		}
	case *SessionSummaryEvent:
		if err := r.store.AddSummary(ctx, e.SessionID, e.Summary, e.FirstKeptEntry); err != nil {
			slog.WarnContext(ctx, "Failed to persist summary", "session_id", e.SessionID, "error", err)
		}
	case *TokenUsageEvent:
		if e.Usage != nil {
			if err := r.store.UpdateSessionTokens(ctx, sessionID, e.Usage.InputTokens, e.Usage.OutputTokens, e.Usage.Cost); err != nil {
				slog.WarnContext(ctx, "Failed to persist token usage", "session_id", sessionID, "error", err)
			}
		}
	case *SessionTitleEvent:
		if err := r.store.UpdateSessionTitle(ctx, sessionID, e.Title); err != nil {
			slog.WarnContext(ctx, "Failed to persist session title", "session_id", sessionID, "error", err)
		}
	case *StreamStoppedEvent:
		r.tracker.mu.Lock()
		r.tracker.remove(sessionID)
		r.tracker.mu.Unlock()
	}
}

func (r *SessionRecorder) addUserMessage(ctx context.Context, e *UserMessageEvent) {
	msg := session.UserMessage(e.Message, e.MultiContent...)
	msg.Kind = e.Kind
	if ps, ok := r.store.(session.PositionalStore); ok {
		if e.SessionPosition < 0 {
			slog.WarnContext(ctx, "Skipping persistence of UserMessageEvent with unknown position", "session_id", e.SessionID, "position", e.SessionPosition)
			return
		}
		if e.Kind == session.MessageKindSubagentEnvelope && r.userMessageExists(ctx, e.SessionID, msg) {
			return
		}
		id, err := ps.AddMessageAt(ctx, e.SessionID, e.SessionPosition, msg)
		if err != nil {
			if errors.Is(err, session.ErrPositionGap) {
				r.appendUserMessage(ctx, e.SessionID, msg, "position gap")
				return
			}
			slog.WarnContext(ctx, "Failed to persist user message at position", "session_id", e.SessionID, "position", e.SessionPosition, "error", err)
			return
		}
		if id == 0 {
			if r.userMessageAtPositionMatches(ctx, e.SessionID, e.SessionPosition, msg) {
				return
			}
			r.appendUserMessage(ctx, e.SessionID, msg, "position collision")
		}
		return
	}
	r.appendUserMessage(ctx, e.SessionID, msg, "")
}

func (r *SessionRecorder) appendUserMessage(ctx context.Context, sessionID string, msg *session.Message, reason string) {
	if reason != "" {
		slog.WarnContext(ctx, "Appending user message after positional persistence failed", "session_id", sessionID, "reason", reason)
	}
	if _, err := r.store.AddMessage(ctx, sessionID, msg); err != nil {
		slog.WarnContext(ctx, "Failed to persist user message", "session_id", sessionID, "error", err)
	}
}

func (r *SessionRecorder) userMessageAtPositionMatches(ctx context.Context, sessionID string, position int, msg *session.Message) bool {
	persisted, err := r.store.GetSession(ctx, sessionID)
	if err != nil || persisted == nil || position < 0 || position >= len(persisted.Messages) {
		if err != nil {
			slog.WarnContext(ctx, "Failed to inspect existing user message position", "session_id", sessionID, "position", position, "error", err)
		}
		return false
	}
	existing := persisted.Messages[position].Message
	if existing == nil {
		return false
	}
	return sameUserMessage(existing, msg)
}

func (r *SessionRecorder) userMessageExists(ctx context.Context, sessionID string, msg *session.Message) bool {
	persisted, err := r.store.GetSession(ctx, sessionID)
	if err != nil || persisted == nil {
		if err != nil {
			slog.WarnContext(ctx, "Failed to inspect existing user messages", "session_id", sessionID, "error", err)
		}
		return false
	}
	for _, item := range persisted.Messages {
		if sameUserMessage(item.Message, msg) {
			return true
		}
	}
	return false
}

func sameUserMessage(a, b *session.Message) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.AgentName == b.AgentName &&
		a.Implicit == b.Implicit &&
		a.Kind == b.Kind &&
		a.Message.Role == b.Message.Role &&
		a.Message.Content == b.Message.Content &&
		reflect.DeepEqual(a.Message.MultiContent, b.Message.MultiContent)
}

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
		id, err := r.store.AddMessage(ctx, sessionID, msg)
		if err != nil {
			slog.WarnContext(ctx, "Failed to create streaming message", "session_id", sessionID, "error", err)
			return
		}
		r.tracker.mu.Lock()
		streaming = r.tracker.get(sessionID)
		if streaming.messageID == 0 {
			streaming.messageID = id
		}
		r.tracker.mu.Unlock()
		return
	}
	if err := r.store.UpdateMessage(ctx, msgID, msg); err != nil {
		slog.WarnContext(ctx, "Failed to update streaming message", "session_id", sessionID, "message_id", msgID, "error", err)
	}
}
