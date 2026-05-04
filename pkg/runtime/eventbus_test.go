package runtime

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAddGlobalObserver_ReceivesEventsAcrossTopics verifies that a global
// observer registered with AddGlobalObserver sees events from multiple
// distinct session topics.
func TestAddGlobalObserver_ReceivesEventsAcrossTopics(t *testing.T) {
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
	ev3 := StreamStopped("session-1", "agent")

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

// TestAddGlobalObserver_MultipleObservers verifies that multiple registered
// observers each receive all events.
func TestAddGlobalObserver_MultipleObservers(t *testing.T) {
	bus := NewEventBus()

	var countA, countB int
	var mu sync.Mutex

	bus.AddGlobalObserver(func(_ string, _ Event) {
		mu.Lock()
		countA++
		mu.Unlock()
	})
	bus.AddGlobalObserver(func(_ string, _ Event) {
		mu.Lock()
		countB++
		mu.Unlock()
	})

	bus.Publish("sess", StreamStarted("sess", "a"))
	bus.Publish("sess", StreamStopped("sess", "a"))

	mu.Lock()
	defer mu.Unlock()

	assert.Equal(t, 2, countA, "first observer should see both events")
	assert.Equal(t, 2, countB, "second observer should see both events")
}

// TestAddGlobalObserver_PanicIsolation verifies that a panicking global
// observer does not prevent subsequent observers from being called, and
// that a panicking observer does not crash the publisher.
func TestAddGlobalObserver_PanicIsolation(t *testing.T) {
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

	// Must not panic.
	require.NotPanics(t, func() {
		bus.Publish("sess", StreamStarted("sess", "a"))
	})

	assert.True(t, received, "observer after a panicking one should still be called")
}

// TestAddGlobalObserver_SeesPreCloseEvent verifies that the global observer
// sees an event published before the topic is closed AND an event published
// after close (since CloseTopic removes the topic from the map and a subsequent
// Publish creates a fresh one that is not closed).
func TestAddGlobalObserver_SeesPreCloseEvent(t *testing.T) {
	bus := NewEventBus()

	var count int
	bus.AddGlobalObserver(func(_ string, _ Event) { count++ })

	bus.Publish("sess", StreamStarted("sess", "a"))
	bus.CloseTopic("sess")

	// CloseTopic removes the topic entry; a subsequent Publish creates a new
	// (non-closed) topic, so the global observer still fires.
	bus.Publish("sess", StreamStopped("sess", "a"))

	assert.Equal(t, 2, count, "global observer should see events both before and after topic close")
}

// TestAddGlobalObserver_NilBusNoPanic verifies nil-safety.
func TestAddGlobalObserver_NilBusNoPanic(t *testing.T) {
	var bus *EventBus
	require.NotPanics(t, func() {
		bus.AddGlobalObserver(func(_ string, _ Event) {})
	})
}
