package runtime

import (
	"log/slog"
	"sync"
)

// EventBus is a session-scoped pub/sub system for runtime events.
// It allows subscribers to receive events published for a specific session,
// with non-blocking publishes that never panic or deadlock.
type EventBus struct {
	mu             sync.RWMutex
	subscribers    map[string][]chan Event    // keyed by sessionID
	closed         map[string]bool            // tracks sessions whose bus has been closed
	reopenWatchers map[string][]chan struct{} // notified when Reopen is called
}

// NewEventBus creates a new EventBus.
func NewEventBus() *EventBus {
	return &EventBus{
		subscribers:    make(map[string][]chan Event),
		closed:         make(map[string]bool),
		reopenWatchers: make(map[string][]chan struct{}),
	}
}

// Subscribe returns a buffered channel that will receive events for the given session.
// The caller should drain this channel and call Unsubscribe when done.
//
// If the session bus was already closed (via Close), Subscribe returns an already-closed
// channel so that callers using `for event := range ch` exit immediately rather than
// blocking forever waiting for events that will never arrive.
func (b *EventBus) Subscribe(sessionID string, bufSize int) <-chan Event {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Late subscriber: session bus already closed — return a pre-closed channel.
	if b.closed[sessionID] {
		ch := make(chan Event, bufSize)
		close(ch)
		return ch
	}

	ch := make(chan Event, bufSize)
	b.subscribers[sessionID] = append(b.subscribers[sessionID], ch)
	return ch
}

// Unsubscribe removes a subscriber channel for the given session.
// Safe to call even if the channel was already removed or closed.
func (b *EventBus) Unsubscribe(sessionID string, ch <-chan Event) {
	b.mu.Lock()
	defer b.mu.Unlock()

	subs, ok := b.subscribers[sessionID]
	if !ok {
		return
	}

	// Find and remove the channel (careful not to modify during iteration)
	var filtered []chan Event
	for _, sub := range subs {
		if sub != ch {
			filtered = append(filtered, sub)
		}
	}

	if len(filtered) == 0 {
		delete(b.subscribers, sessionID)
	} else {
		b.subscribers[sessionID] = filtered
	}
}

// Publish sends an event to all subscribers for the given session.
// Publish never blocks and never panics — if a subscriber channel is full,
// the event is dropped for that subscriber and a debug log is emitted.
//
// Thread safety: Publish holds RLock for the entire duration of sending.
// Close() holds WLock when closing channels, so channels are never closed
// while Publish is sending. The non-blocking select ensures Publish never
// blocks while holding the lock.
func (b *EventBus) Publish(sessionID string, event Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	subs, ok := b.subscribers[sessionID]
	if !ok || len(subs) == 0 {
		return
	}

	// Send to each subscriber with best-effort semantics.
	for _, sub := range subs {
		select {
		case sub <- event:
			// Event sent successfully
		default:
			// Channel is full — drop the event (best-effort).
			slog.Debug("EventBus: subscriber channel full, dropping event",
				"session_id", sessionID, "event_type", getEventType(event))
		}
	}
}

// Close removes all subscriber channels for the given session and closes them.
// After Close returns, no further events will be published for this session.
// Subscribers detect stream end by reading from their closed channel.
//
// Thread safety: channels are closed under the write lock so that Publish
// (which holds a read lock while sending) cannot race with the close.
//
// Late subscribers: after Close is called, any subsequent Subscribe call for
// the same sessionID will return an already-closed channel (see Subscribe).
func (b *EventBus) Close(sessionID string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	subs, ok := b.subscribers[sessionID]
	if !ok {
		// Record closure even if there were no subscribers, so late subscribers
		// (e.g. the TUI tab opening after delegation completes) get a closed channel.
		b.closed[sessionID] = true
		return
	}
	delete(b.subscribers, sessionID)
	// Record that this session bus has been closed so Subscribe handles late arrivals.
	b.closed[sessionID] = true

	// Close all channels under the lock. Publish holds RLock during send,
	// so it cannot observe a closed channel while we hold the write lock.
	for _, ch := range subs {
		close(ch)
	}
}

// Reopen clears the closed latch for a session so a new run can create fresh
// subscriptions and publish events again after a previous Close.
// Also signals all registered reopen watchers (non-blocking).
func (b *EventBus) Reopen(sessionID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.closed, sessionID)
	// Signal reopen watchers (non-blocking)
	for _, ch := range b.reopenWatchers[sessionID] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// WatchReopens returns a channel that receives a signal each time Reopen is
// called for the given session. The caller should call UnwatchReopens when done.
func (b *EventBus) WatchReopens(sessionID string) <-chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	ch := make(chan struct{}, 4) // buffered to avoid blocking Reopen
	b.reopenWatchers[sessionID] = append(b.reopenWatchers[sessionID], ch)
	return ch
}

// UnwatchReopens removes a previously registered reopen watcher channel.
func (b *EventBus) UnwatchReopens(sessionID string, ch <-chan struct{}) {
	b.mu.Lock()
	defer b.mu.Unlock()
	watchers := b.reopenWatchers[sessionID]
	var filtered []chan struct{}
	for _, w := range watchers {
		if w != ch {
			filtered = append(filtered, w)
		}
	}
	if len(filtered) == 0 {
		delete(b.reopenWatchers, sessionID)
	} else {
		b.reopenWatchers[sessionID] = filtered
	}
}

// getEventType returns a friendly string name for the event type.
func getEventType(event Event) string {
	switch event.(type) {
	case *UserMessageEvent:
		return "UserMessage"
	case *AgentChoiceEvent:
		return "AgentChoice"
	case *AgentChoiceReasoningEvent:
		return "AgentChoiceReasoning"
	case *MessageAddedEvent:
		return "MessageAdded"
	case *TokenUsageEvent:
		return "TokenUsage"
	case *SessionTitleEvent:
		return "SessionTitle"
	case *ErrorEvent:
		return "Error"
	case *StreamStartedEvent:
		return "StreamStarted"
	case *StreamStoppedEvent:
		return "StreamStopped"
	default:
		return "Unknown"
	}
}
