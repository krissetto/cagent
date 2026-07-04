package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Attaching mid-stream: the subscriber receives the in-flight assistant
// message's head as seed (captured atomically with the subscription) and its
// tail through the channel — together exactly the full message. Boundary
// events reset the in-flight state so later subscribers get no stale prefix.
func TestSessionEventHubSeedsInflightAssistant(t *testing.T) {
	t.Parallel()

	h := newSessionEventHub()

	// A run streams the first half of a message before anyone watches.
	h.Publish("s1", AgentChoiceReasoning("planner", "s1", "thinking… "))
	h.Publish("s1", AgentChoice("planner", "s1", "Hello, "))
	h.Publish("s1", AgentChoice("planner", "s1", "wor"))

	seed, ch, cancel := h.Subscribe("s1", 16)
	defer cancel()

	require.Len(t, seed, 2)
	reasoning, ok := seed[0].(*AgentChoiceReasoningEvent)
	require.True(t, ok)
	assert.Equal(t, "thinking… ", reasoning.Content)
	assert.Equal(t, "planner", reasoning.AgentName)
	head, ok := seed[1].(*AgentChoiceEvent)
	require.True(t, ok)
	assert.Equal(t, "Hello, wor", head.Content)

	// The rest of the message arrives on the channel only.
	h.Publish("s1", AgentChoice("planner", "s1", "ld!"))
	tail := (<-ch).(*AgentChoiceEvent)
	assert.Equal(t, "ld!", tail.Content)

	// Commit boundary resets the in-flight state: a later subscriber gets no
	// seed for content that is already in the session transcript.
	h.Publish("s1", &MessageAddedEvent{SessionID: "s1"})
	seed2, _, cancel2 := h.Subscribe("s1", 16)
	defer cancel2()
	assert.Empty(t, seed2, "committed content must not be re-seeded")
}

// A stopped or errored run must not leak a stale prefix to later subscribers.
func TestSessionEventHubResetsInflightOnBoundaries(t *testing.T) {
	t.Parallel()

	for _, boundary := range []Event{
		&StreamStoppedEvent{},
		&ErrorEvent{},
		UserMessage("hi", "s1", nil),
	} {
		h := newSessionEventHub()
		h.Publish("s1", AgentChoice("planner", "s1", "partial"))
		h.Publish("s1", boundary)
		seed, _, cancel := h.Subscribe("s1", 1)
		assert.Empty(t, seed, "boundary %T must reset the in-flight prefix", boundary)
		cancel()
	}
}

// Idle attach: no in-flight content, no seed.
func TestSessionEventHubNoSeedWhenIdle(t *testing.T) {
	t.Parallel()

	h := newSessionEventHub()
	seed, _, cancel := h.Subscribe("s1", 1)
	defer cancel()
	assert.Empty(t, seed)
}

// Attaching mid-run: the subscriber is seeded with a synthetic StreamStarted
// so viewers (tab spinners, working state) know a run is live even though its
// real start predates the subscription. The seed comes before any in-flight
// content, and stops/starts keep later subscribers accurate.
func TestSessionEventHubSeedsLiveRun(t *testing.T) {
	t.Parallel()

	h := newSessionEventHub()

	// Idle session: no lifecycle seed.
	seed, _, cancel := h.Subscribe("s1", 16)
	cancel()
	assert.Empty(t, seed, "idle session must not be seeded with a run start")

	// A run starts and streams before anyone watches.
	h.Publish("s1", StreamStarted("s1", "coder"))
	h.Publish("s1", AgentChoice("coder", "s1", "Working…"))

	seed, _, cancel = h.Subscribe("s1", 16)
	cancel()
	require.Len(t, seed, 2)
	started, ok := seed[0].(*StreamStartedEvent)
	require.True(t, ok, "the run's start is seeded first")
	assert.Equal(t, "s1", started.SessionID)
	assert.Equal(t, "coder", started.AgentName)
	_, ok = seed[1].(*AgentChoiceEvent)
	require.True(t, ok, "in-flight content follows the run start")

	// The run stops: later subscribers see no live run.
	h.Publish("s1", StreamStopped("s1", "coder", ""))
	seed, _, cancel = h.Subscribe("s1", 16)
	cancel()
	assert.Empty(t, seed, "a stopped run must not be seeded")

	// A fresh run makes the session live again.
	h.Publish("s1", StreamStarted("s1", "coder"))
	seed, _, cancel = h.Subscribe("s1", 16)
	cancel()
	require.Len(t, seed, 1)
	assert.IsType(t, &StreamStartedEvent{}, seed[0])
}
