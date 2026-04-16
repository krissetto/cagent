package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestEventBus_WatchReopens tests the WatchReopens mechanism for resubscription after Close/Reopen
func TestEventBus_WatchReopens(t *testing.T) {
	bus := NewEventBus()

	// Watch for reopens
	reopens := bus.WatchReopens("sess-1")

	// Close the session
	bus.Close("sess-1")

	// Subscribe — should get a pre-closed channel
	ch := bus.Subscribe("sess-1", 10)
	_, ok := <-ch
	assert.False(t, ok, "channel should be pre-closed after Close")

	// Reopen
	bus.Reopen("sess-1")

	// Should receive a signal on reopens
	select {
	case <-reopens:
		// OK
	default:
		t.Fatal("expected reopen signal")
	}

	// Subscribe again — should get an open channel
	ch2 := bus.Subscribe("sess-1", 10)
	bus.Publish("sess-1", &UserMessageEvent{})

	select {
	case event, ok := <-ch2:
		assert.True(t, ok, "resubscribed channel should be open")
		assert.NotNil(t, event)
	default:
		t.Fatal("expected event on re-subscribed channel")
	}

	// Cleanup
	bus.UnwatchReopens("sess-1", reopens)
	bus.Close("sess-1")
}

// TestEventBus_UnwatchReopens verifies that UnwatchReopens removes the watcher
func TestEventBus_UnwatchReopens(t *testing.T) {
	bus := NewEventBus()

	reopens1 := bus.WatchReopens("sess-1")
	reopens2 := bus.WatchReopens("sess-1")

	// Unwatch the first one
	bus.UnwatchReopens("sess-1", reopens1)

	// Reopen should only signal reopens2
	bus.Reopen("sess-1")

	// reopens1 should not receive a signal
	select {
	case <-reopens1:
		t.Fatal("unwatched channel should not receive signal")
	default:
		// Expected
	}

	// reopens2 should receive a signal
	select {
	case <-reopens2:
		// OK
	default:
		t.Fatal("watched channel should receive signal")
	}

	bus.UnwatchReopens("sess-1", reopens2)
}
