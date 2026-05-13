package runtime

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEventBus_SubscribePublishFanOut verifies that a single Publish is
// fanned out to every subscriber of the session.
func TestEventBus_SubscribePublishFanOut(t *testing.T) {
	bus := NewEventBus()
	ctx := t.Context()

	subA := bus.Subscribe(ctx, "sess", 8)
	subB := bus.Subscribe(ctx, "sess", 8)
	t.Cleanup(subA.Cancel)
	t.Cleanup(subB.Cancel)

	ev := StreamStarted("sess", "agent")
	bus.Publish("sess", ev)

	for _, sub := range []*Subscription{subA, subB} {
		select {
		case got := <-sub.Events:
			assert.Equal(t, ev, got)
		case <-time.After(time.Second):
			t.Fatalf("subscriber timed out waiting for event")
		}
	}
}

// TestEventBus_OnlyDeliversToMatchingTopic ensures events for one session
// are not delivered to another session's subscribers.
func TestEventBus_OnlyDeliversToMatchingTopic(t *testing.T) {
	bus := NewEventBus()
	ctx := t.Context()

	subA := bus.Subscribe(ctx, "session-a", 8)
	subB := bus.Subscribe(ctx, "session-b", 8)
	t.Cleanup(subA.Cancel)
	t.Cleanup(subB.Cancel)

	bus.Publish("session-a", StreamStarted("session-a", "agent"))

	select {
	case got := <-subA.Events:
		_, ok := got.(*StreamStartedEvent)
		assert.True(t, ok, "session-a subscriber should receive its event")
	case <-time.After(time.Second):
		t.Fatalf("session-a subscriber timed out")
	}

	select {
	case got := <-subB.Events:
		t.Fatalf("session-b subscriber received an event meant for session-a: %#v", got)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestEventBus_BufferFullDropsOnSubscriber verifies that a slow subscriber
// drops events instead of blocking the publisher.
func TestEventBus_BufferFullDropsOnSubscriber(t *testing.T) {
	bus := NewEventBus()
	ctx := t.Context()

	sub := bus.Subscribe(ctx, "sess", 1) // tiny buffer to force overflow
	t.Cleanup(sub.Cancel)

	// First publish fills the buffer.
	bus.Publish("sess", StreamStarted("sess", "agent"))
	// Subsequent publishes have nowhere to go and must be dropped.
	for range 10 {
		bus.Publish("sess", StreamStopped("sess", "agent", ""))
	}

	assert.GreaterOrEqual(t, sub.Dropped.Load(), int64(1),
		"slow subscriber should record dropped events instead of blocking the publisher")
}

// TestEventBus_CancelStopsDelivery verifies Subscription.Cancel removes the
// subscriber and closes its channel.
func TestEventBus_CancelStopsDelivery(t *testing.T) {
	bus := NewEventBus()
	ctx := t.Context()

	sub := bus.Subscribe(ctx, "sess", 8)
	bus.Publish("sess", StreamStarted("sess", "agent"))
	require.NotNil(t, <-sub.Events)

	sub.Cancel()
	// channel must close
	select {
	case _, ok := <-sub.Events:
		assert.False(t, ok, "events channel must be closed after Cancel")
	case <-time.After(time.Second):
		t.Fatalf("events channel was not closed after Cancel")
	}

	// subsequent publish must not panic and must be a no-op for this sub.
	require.NotPanics(t, func() {
		bus.Publish("sess", StreamStopped("sess", "agent", ""))
	})
}

// TestEventBus_CloseTopicClosesAllSubscribers verifies CloseTopic closes
// every subscriber's channel for the session.
func TestEventBus_CloseTopicClosesAllSubscribers(t *testing.T) {
	bus := NewEventBus()
	ctx := t.Context()

	subA := bus.Subscribe(ctx, "sess", 8)
	subB := bus.Subscribe(ctx, "sess", 8)

	bus.CloseTopic("sess")

	for _, sub := range []*Subscription{subA, subB} {
		select {
		case _, ok := <-sub.Events:
			assert.False(t, ok, "events channel must be closed after CloseTopic")
		case <-time.After(time.Second):
			t.Fatalf("events channel was not closed after CloseTopic")
		}
	}
}

// TestEventBus_SubscribeWithSnapshot_NoGap verifies that a late attacher
// receives the in-progress streaming snapshot and the very next live event
// has no overlap and no gap with it.
func TestEventBus_SubscribeWithSnapshot_NoGap(t *testing.T) {
	bus := NewEventBus()
	ctx := t.Context()

	// Stream some assistant content before the subscriber attaches.
	bus.Publish("sess", AgentChoice("agent", "sess", "Hello, "))
	bus.Publish("sess", AgentChoice("agent", "sess", "world"))

	sub, snapshot := bus.SubscribeWithSnapshot(ctx, "sess", 8)
	t.Cleanup(sub.Cancel)

	assert.True(t, snapshot.HasContent(), "snapshot must carry pre-attach content")
	assert.Equal(t, "Hello, world", snapshot.Content)
	assert.Equal(t, "agent", snapshot.AgentName)

	// Live tail begins after the snapshot. Sending another delta must arrive
	// on the subscription channel.
	bus.Publish("sess", AgentChoice("agent", "sess", "!"))

	select {
	case got := <-sub.Events:
		ace, ok := got.(*AgentChoiceEvent)
		require.True(t, ok)
		assert.Equal(t, "!", ace.Content, "first live event must be the next delta after the snapshot")
	case <-time.After(time.Second):
		t.Fatalf("subscriber timed out waiting for live tail event")
	}
}

// TestEventBus_SubscribeWithSnapshot_ResetOnMessageAdded verifies that the
// streaming snapshot resets after a MessageAddedEvent so that a subscriber
// attaching between turns does not see stale content.
func TestEventBus_SubscribeWithSnapshot_ResetOnMessageAdded(t *testing.T) {
	bus := NewEventBus()
	ctx := t.Context()

	bus.Publish("sess", AgentChoice("agent", "sess", "old "))
	bus.Publish("sess", AgentChoice("agent", "sess", "content"))
	// Finalising the message must reset the snapshot.
	bus.Publish("sess", MessageAdded("sess", nil, "agent"))

	sub, snapshot := bus.SubscribeWithSnapshot(ctx, "sess", 8)
	t.Cleanup(sub.Cancel)

	assert.False(t, snapshot.HasContent(),
		"snapshot must be empty after MessageAddedEvent")
}

// TestEventBus_AddGlobalObserver_ReceivesAcrossTopics verifies that a global
// observer registered with AddGlobalObserver sees events from multiple
// distinct session topics.
func TestEventBus_AddGlobalObserver_ReceivesAcrossTopics(t *testing.T) {
	bus := NewEventBus()

	var mu sync.Mutex
	type seen struct {
		sessionID string
		ev        Event
	}
	var received []seen

	bus.AddGlobalObserver(func(sessionID string, ev Event) {
		mu.Lock()
		defer mu.Unlock()
		received = append(received, seen{sessionID: sessionID, ev: ev})
	})

	ev1 := StreamStarted("session-1", "agent")
	ev2 := StreamStarted("session-2", "agent")
	ev3 := StreamStopped("session-1", "agent", "")

	bus.Publish("session-1", ev1)
	bus.Publish("session-2", ev2)
	bus.Publish("session-1", ev3)

	mu.Lock()
	defer mu.Unlock()

	require.Len(t, received, 3)
	assert.Equal(t, "session-1", received[0].sessionID)
	assert.Equal(t, ev1, received[0].ev)
	assert.Equal(t, "session-2", received[1].sessionID)
	assert.Equal(t, ev2, received[1].ev)
	assert.Equal(t, "session-1", received[2].sessionID)
	assert.Equal(t, ev3, received[2].ev)
}

// TestEventBus_AddGlobalObserver_PanicIsolation verifies that a panicking
// global observer does not prevent subsequent observers from being called,
// and that a panicking observer does not crash the publisher.
func TestEventBus_AddGlobalObserver_PanicIsolation(t *testing.T) {
	bus := NewEventBus()

	var received bool

	// Observer that panics.
	bus.AddGlobalObserver(func(_ string, _ Event) {
		panic("test observer panic")
	})

	// Observer registered after the panicky one must still fire.
	bus.AddGlobalObserver(func(_ string, _ Event) {
		received = true
	})

	require.NotPanics(t, func() {
		bus.Publish("sess", StreamStarted("sess", "a"))
	})

	assert.True(t, received, "observer after a panicking one should still be called")
}

// TestEventBus_NilSafety verifies the bus tolerates nil-receiver and
// nil-argument calls so callers don't have to guard them.
func TestEventBus_NilSafety(t *testing.T) {
	var bus *EventBus
	require.NotPanics(t, func() {
		bus.AddGlobalObserver(func(_ string, _ Event) {})
	})
	require.NotPanics(t, func() {
		bus.Publish("sess", StreamStarted("sess", "a"))
	})

	// Real bus tolerates empty session id and nil event.
	realBus := NewEventBus()
	require.NotPanics(t, func() {
		realBus.Publish("", StreamStarted("sess", "a"))
		realBus.Publish("sess", nil)
	})
}

// TestEventBus_RaceFanOut exercises concurrent publishers, subscribers, and
// cancellations to flush out races under -race.
func TestEventBus_RaceFanOut(t *testing.T) {
	bus := NewEventBus()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	const subscribers = 8
	const publishers = 4
	const eventsPerPublisher = 100

	var wg sync.WaitGroup
	subs := make([]*Subscription, subscribers)
	for i := range subscribers {
		sub := bus.Subscribe(ctx, "sess", 32)
		subs[i] = sub
		wg.Go(func() {
			for range sub.Events {
				// drain
			}
		})
	}

	var globalCount atomic.Int64
	bus.AddGlobalObserver(func(_ string, _ Event) {
		globalCount.Add(1)
	})

	var pubWG sync.WaitGroup
	pubWG.Add(publishers)
	for range publishers {
		go func() {
			defer pubWG.Done()
			for range eventsPerPublisher {
				bus.Publish("sess", StreamStarted("sess", "agent"))
			}
		}()
	}
	pubWG.Wait()

	// Cancel half of the subscribers concurrently with CloseTopic.
	for i := range subscribers / 2 {
		go subs[i].Cancel()
	}
	bus.CloseTopic("sess")

	wg.Wait()
	assert.Equal(t, int64(publishers*eventsPerPublisher), globalCount.Load(),
		"every publish must reach the global observer")
}
