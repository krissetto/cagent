package runtime

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
)

// EventBus is a per-session event fan-out that lets multiple independent
// observers attach to the same live session (root or subagent) and receive
// every event that session emits.
//
// Two kinds of observers are supported:
//
//   - Per-topic subscribers, registered via [EventBus.Subscribe] or
//     [EventBus.SubscribeWithSnapshot], receive events for one specific
//     session id. Each subscriber owns a buffered channel; if the channel is
//     full when a publish occurs the event is dropped for that subscriber
//     (and counted via [Subscription.Dropped]) — the publisher is never
//     blocked.
//   - Global observers, registered via [EventBus.AddGlobalObserver], receive
//     every event published on any topic. They run synchronously after the
//     per-topic fan-out, in registration order, with each call wrapped in a
//     panic recovery so a misbehaving observer cannot stall the publisher.
//     The [SessionRecorder] is registered as a global observer so it can
//     persist all sessions through a single sink.
//
// In addition to fan-out, each topic accumulates a tiny streaming snapshot
// (the in-progress assistant + reasoning text) so a late-attaching
// subscriber can request its current state via [EventBus.SubscribeWithSnapshot]
// and replay the deltas it missed without waiting for the turn to finish.
// Note: the streaming snapshot is intentionally minimal; PR4 layers a
// store-backed full-session replay on top to give the live-attach UX
// guarantee of "full session on attach".
type EventBus struct {
	mu              sync.Mutex
	topics          map[string]*eventTopic
	globalObservers []func(sessionID string, ev Event)
}

type eventTopic struct {
	mu          sync.Mutex
	subscribers []*Subscription
	closed      bool

	// streaming holds the in-progress assistant turn for the topic. It is
	// updated by [EventBus.Publish] for each AgentChoice / AgentChoiceReasoning
	// event and reset on MessageAdded / StreamStopped, all under topic.mu.
	streaming streamingSnapshot
}

// streamingSnapshot is the per-topic streaming buffer captured by Publish.
// It is the minimum needed to let a late-attaching subscriber render the
// content it missed: the agent name, accumulated assistant content, and
// accumulated reasoning content. We keep separate buffers so reasoning and
// assistant content can be replayed in their own dedicated message blocks.
type streamingSnapshot struct {
	agentName        string
	content          strings.Builder
	reasoningContent strings.Builder
	active           bool
}

func (s *streamingSnapshot) reset() {
	s.agentName = ""
	s.content.Reset()
	s.reasoningContent.Reset()
	s.active = false
}

// StreamingSnapshot is the public, copyable view of an [eventTopic.streaming].
// It is what [EventBus.SubscribeWithSnapshot] hands back so callers can
// re-emit the in-progress content immediately on attach.
type StreamingSnapshot struct {
	AgentName        string
	Content          string
	ReasoningContent string
}

// HasContent reports whether the snapshot carries any replayable text.
func (s StreamingSnapshot) HasContent() bool {
	return s.Content != "" || s.ReasoningContent != ""
}

// Subscription is returned by [EventBus.Subscribe]. Callers must invoke
// Cancel when finished to release resources.
type Subscription struct {
	// SessionID is the id of the session this subscription observes.
	SessionID string
	// Events streams events emitted on the session. The channel is
	// closed when the subscription is cancelled or the topic is
	// explicitly closed via [EventBus.CloseTopic].
	Events <-chan Event
	// Cancel unregisters the subscription and closes the events channel.
	// Safe to call multiple times.
	Cancel func()
	// Dropped counts events that could not be delivered because the
	// subscription's buffer was full when a publish occurred.
	Dropped *atomic.Int64

	events  chan Event
	closed  atomic.Bool
	topic   *eventTopic
	release func()
}

// NewEventBus builds an empty bus.
func NewEventBus() *EventBus {
	return &EventBus{topics: make(map[string]*eventTopic)}
}

// Publish delivers ev to every current subscriber of the given session and
// to every registered global observer.
//
// Per-topic deliveries are non-blocking: if a subscriber's buffer is full,
// the event is dropped for that subscriber and its [Subscription.Dropped]
// counter increments. Other subscribers and global observers still receive
// the event.
//
// Publishing to a closed topic is a no-op for per-topic subscribers, but
// global observers still see the event since they are not gated by the
// topic.
//
// The topic mutex is held for the full duration of the per-topic fan-out.
// That is intentional: sends are non-blocking (select-default), so the lock
// is only held for a tiny slice of time, and serialising publishes with
// [Subscription.cancel] closes a correctness hole that otherwise lets a
// concurrent cancel close the events channel mid-send — which panics (and
// trips the race detector well before that).
func (b *EventBus) Publish(sessionID string, ev Event) {
	if b == nil || sessionID == "" || ev == nil {
		return
	}
	b.mu.Lock()
	t, ok := b.topics[sessionID]
	if !ok {
		// Lazy-create the topic so the per-session streaming snapshot is
		// accumulated even before the first subscriber attaches. Without
		// this, the bus would silently drop pre-attach events and the
		// snapshot would always be empty for late attachers.
		t = &eventTopic{}
		b.topics[sessionID] = t
	}
	// Snapshot the global observer slice under the bus lock so registration
	// can race with a concurrent Publish without panicking. We invoke the
	// observers *after* per-topic fan-out, outside any topic lock.
	observers := b.globalObservers
	b.mu.Unlock()

	t.mu.Lock()
	if !t.closed {
		trackStreamingSnapshot(&t.streaming, ev)
		for _, sub := range t.subscribers {
			if sub.closed.Load() {
				// Cancel raced us between acquiring the topic lock and this
				// iteration's read — skip so we don't send on a doomed channel.
				continue
			}
			select {
			case sub.events <- ev:
			default:
				sub.Dropped.Add(1)
			}
		}
	}
	t.mu.Unlock()

	// Invoke global observers outside the topic lock. A misbehaving observer
	// must not block per-topic subscribers or other observers, so we recover
	// from panics and log a warning.
	for _, fn := range observers {
		invokeGlobalObserver(fn, sessionID, ev)
	}
}

// AddGlobalObserver registers fn to receive every event published on any
// topic. Observers are invoked after per-topic fan-out, in registration
// order. Panicking observers are recovered from and a warning is logged so
// they cannot stall the publish path.
//
// The runtime's [SessionRecorder] is registered via this hook so all session
// state changes flow through a single global writer.
func (b *EventBus) AddGlobalObserver(fn func(sessionID string, ev Event)) {
	if b == nil || fn == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.globalObservers = append(b.globalObservers, fn)
}

func invokeGlobalObserver(fn func(string, Event), sessionID string, ev Event) {
	defer func() {
		if rec := recover(); rec != nil {
			slog.Warn("global event observer panicked", "session_id", sessionID, "panic", rec)
		}
	}()
	fn(sessionID, ev)
}

func trackStreamingSnapshot(snapshot *streamingSnapshot, ev Event) {
	switch e := ev.(type) {
	case *AgentChoiceEvent:
		if e.Content == "" {
			return
		}
		if snapshot.agentName == "" {
			snapshot.agentName = e.AgentName
		}
		snapshot.content.WriteString(e.Content)
		snapshot.active = true
	case *AgentChoiceReasoningEvent:
		if e.Content == "" {
			return
		}
		if snapshot.agentName == "" {
			snapshot.agentName = e.AgentName
		}
		snapshot.reasoningContent.WriteString(e.Content)
		snapshot.active = true
	case *MessageAddedEvent, *StreamStoppedEvent:
		snapshot.reset()
	}
}

func exportStreamingSnapshot(snapshot streamingSnapshot) StreamingSnapshot {
	if !snapshot.active {
		return StreamingSnapshot{}
	}
	return StreamingSnapshot{
		AgentName:        snapshot.agentName,
		Content:          snapshot.content.String(),
		ReasoningContent: snapshot.reasoningContent.String(),
	}
}

// SubscribeWithSnapshot behaves like [EventBus.Subscribe] but also returns
// the topic's current in-progress streaming snapshot, captured while the
// new subscriber is registered under the same topic lock. That guarantees
// the caller sees a consistent handoff: no overlap and no gap between the
// returned snapshot and the first live event delivered on sub.Events.
//
// PR4 will layer a store-backed full-session prefix on top of the snapshot
// returned here so live-attach can present the entire transcript plus
// gap-free live tail.
func (b *EventBus) SubscribeWithSnapshot(ctx context.Context, sessionID string, buffer int) (*Subscription, StreamingSnapshot) {
	if buffer <= 0 {
		buffer = defaultEventChannelCapacity
	}

	b.mu.Lock()
	t, ok := b.topics[sessionID]
	if !ok {
		t = &eventTopic{}
		b.topics[sessionID] = t
	}
	b.mu.Unlock()

	events := make(chan Event, buffer)
	sub := &Subscription{
		SessionID: sessionID,
		Events:    events,
		events:    events,
		Dropped:   new(atomic.Int64),
		topic:     t,
	}
	sub.Cancel = sub.cancel
	sub.release = func() { b.removeSubscriber(sessionID, sub) }

	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		sub.cancel()
		return sub, StreamingSnapshot{}
	}
	t.subscribers = append(t.subscribers, sub)
	snapshot := exportStreamingSnapshot(t.streaming)
	t.mu.Unlock()

	if ctx != nil {
		go func() {
			<-ctx.Done()
			sub.cancel()
		}()
	}

	return sub, snapshot
}

// Subscribe registers a new observer of the given session id. The
// subscription is valid until the caller invokes Cancel, the topic is
// closed by [EventBus.CloseTopic], or ctx is cancelled.
//
// buffer controls how many events can be queued per subscription.
// A value of <= 0 defaults to [defaultEventChannelCapacity].
func (b *EventBus) Subscribe(ctx context.Context, sessionID string, buffer int) *Subscription {
	sub, _ := b.SubscribeWithSnapshot(ctx, sessionID, buffer)
	return sub
}

// CloseTopic closes the topic for the given session id. All current
// subscribers' events channels are closed and further publications to
// the topic become no-ops for per-topic fan-out. Global observers
// continue to receive any subsequent events for the session.
//
// The runtime invokes this when a session has permanently finished
// (e.g. a subagent reaches a terminal state, or a root session is
// fully drained at the end of RunStream).
func (b *EventBus) CloseTopic(sessionID string) {
	b.mu.Lock()
	t, ok := b.topics[sessionID]
	if ok {
		delete(b.topics, sessionID)
	}
	b.mu.Unlock()
	if !ok {
		return
	}
	t.mu.Lock()
	t.closed = true
	subs := t.subscribers
	t.subscribers = nil
	// Close each surviving subscription's channel while we still hold the
	// topic lock. Any in-flight Publish on this topic also takes t.mu, so
	// holding it here guarantees no concurrent send can race the close.
	for _, sub := range subs {
		if sub.closed.Swap(true) {
			continue
		}
		close(sub.events)
	}
	t.mu.Unlock()
}

// ActiveTopics returns the set of session ids that currently have a
// retained topic (subscribers attached or pre-attach snapshot accumulated).
// Intended for diagnostics.
func (b *EventBus) ActiveTopics() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, 0, len(b.topics))
	for id := range b.topics {
		out = append(out, id)
	}
	return out
}

func (b *EventBus) removeSubscriber(sessionID string, sub *Subscription) {
	b.mu.Lock()
	t, ok := b.topics[sessionID]
	b.mu.Unlock()
	if !ok {
		return
	}
	t.mu.Lock()
	for i, s := range t.subscribers {
		if s == sub {
			t.subscribers = append(t.subscribers[:i], t.subscribers[i+1:]...)
			break
		}
	}
	t.mu.Unlock()
}

func (s *Subscription) cancel() {
	if s.closed.Swap(true) {
		return
	}
	if s.release != nil {
		s.release()
	}
	// Serialise the close with any concurrent Publish on the same topic.
	// Publish holds s.topic.mu during its fan-out loop (see the comment on
	// Publish for the full rationale); taking it here means close(s.events)
	// is guaranteed to run only after any in-flight send has completed.
	if s.topic != nil {
		s.topic.mu.Lock()
		defer s.topic.mu.Unlock()
	}
	close(s.events)
}
