package runtime

import (
	"context"
	"sync"
	"sync/atomic"
)

type EventSubscription struct {
	mu      sync.Mutex
	events  chan Event
	dropped atomic.Int64
	cancel  context.CancelFunc
	closed  bool
}

func (s *EventSubscription) Events() <-chan Event { return s.events }
func (s *EventSubscription) Dropped() int64       { return s.dropped.Load() }
func (s *EventSubscription) Close() {
	if s.cancel != nil {
		s.cancel()
	}
}

func (s *EventSubscription) send(ev Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	select {
	case s.events <- ev:
	default:
		s.dropped.Add(1)
	}
}

func (s *EventSubscription) closeEvents() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	close(s.events)
}

type eventTopic struct {
	subscribers map[*EventSubscription]struct{}
	snapshot    []Event
	sequence    uint64
	closed      bool
}

type EventBus struct {
	mu              sync.RWMutex
	topics          map[string]*eventTopic
	globalObservers []func(string, Event)
}

func NewEventBus() *EventBus {
	return &EventBus{topics: make(map[string]*eventTopic)}
}

func (b *EventBus) AddGlobalObserver(observer func(string, Event)) {
	if b == nil || observer == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.globalObservers = append(b.globalObservers, observer)
}

func (b *EventBus) Publish(sessionID string, event Event) {
	if b == nil || sessionID == "" || event == nil {
		return
	}

	b.mu.Lock()
	topic := b.topicLocked(sessionID)
	if topic.closed {
		b.mu.Unlock()
		return
	}
	topic.sequence++
	topic.snapshot = append(topic.snapshot, event)
	subs := make([]*EventSubscription, 0, len(topic.subscribers))
	for sub := range topic.subscribers {
		subs = append(subs, sub)
	}
	observers := append([]func(string, Event){}, b.globalObservers...)
	b.mu.Unlock()

	for _, observer := range observers {
		observer(sessionID, event)
	}
	for _, sub := range subs {
		sub.send(event)
	}
}

func (b *EventBus) Subscribe(ctx context.Context, sessionID string, buffer int) *EventSubscription {
	sub, _ := b.subscribeWithSequence(ctx, sessionID, buffer)
	return sub
}

func (b *EventBus) SubscribeWithSnapshot(ctx context.Context, sessionID string, buffer int) (*EventSubscription, []Event) {
	sub, snapshot := b.subscribeWithSequence(ctx, sessionID, buffer)
	return sub, snapshot
}

func (b *EventBus) StreamingSnapshot(sessionID string) []Event {
	if b == nil || sessionID == "" {
		return nil
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	topic := b.topics[sessionID]
	if topic == nil {
		return nil
	}
	return append([]Event(nil), topic.snapshot...)
}

func (b *EventBus) TopicSequence(sessionID string) uint64 {
	if b == nil || sessionID == "" {
		return 0
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	topic := b.topics[sessionID]
	if topic == nil {
		return 0
	}
	return topic.sequence
}

func (b *EventBus) CloseTopic(sessionID string) {
	if b == nil || sessionID == "" {
		return
	}
	b.mu.Lock()
	topic := b.topics[sessionID]
	if topic == nil || topic.closed {
		b.mu.Unlock()
		return
	}
	topic.closed = true
	subs := make([]*EventSubscription, 0, len(topic.subscribers))
	for sub := range topic.subscribers {
		subs = append(subs, sub)
		delete(topic.subscribers, sub)
	}
	b.mu.Unlock()
	for _, sub := range subs {
		sub.closeEvents()
	}
}

func (b *EventBus) subscribeWithSequence(ctx context.Context, sessionID string, buffer int) (*EventSubscription, []Event) {
	if buffer <= 0 {
		buffer = defaultEventChannelCapacity
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	sub := &EventSubscription{events: make(chan Event, buffer), cancel: cancel}

	b.mu.Lock()
	topic := b.topicLocked(sessionID)
	if topic.closed {
		sub.closeEvents()
		b.mu.Unlock()
		return sub, nil
	}
	topic.subscribers[sub] = struct{}{}
	snapshot := append([]Event(nil), topic.snapshot...)
	b.mu.Unlock()

	go func() {
		<-ctx.Done()
		b.mu.Lock()
		topic := b.topics[sessionID]
		if topic == nil {
			b.mu.Unlock()
			return
		}
		if _, ok := topic.subscribers[sub]; ok {
			delete(topic.subscribers, sub)
			b.mu.Unlock()
			sub.closeEvents()
			return
		}
		b.mu.Unlock()
	}()

	return sub, snapshot
}

func (b *EventBus) topicLocked(sessionID string) *eventTopic {
	topic := b.topics[sessionID]
	if topic == nil {
		topic = &eventTopic{subscribers: make(map[*EventSubscription]struct{})}
		b.topics[sessionID] = topic
	}
	return topic
}
