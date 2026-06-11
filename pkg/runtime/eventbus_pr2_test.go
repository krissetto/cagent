package runtime

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
)

func TestEventBusPublishSubscribeDropCountingAndObservers(t *testing.T) {
	bus := NewEventBus()
	observed := make(chan Event, 2)
	bus.AddGlobalObserver(func(sessionID string, ev Event) {
		assert.Equal(t, "sess", sessionID)
		observed <- ev
	})

	sub := bus.Subscribe(t.Context(), "sess", 1)
	first := Warning("first", "agent")
	second := Warning("second", "agent")
	bus.Publish("sess", first)
	bus.Publish("sess", second)

	assert.Same(t, first, <-observed)
	assert.Same(t, second, <-observed)
	assert.Same(t, first, <-sub.Events())
	assert.Equal(t, int64(1), sub.Dropped())
}

func TestEventBusSnapshotAndCloseTopic(t *testing.T) {
	bus := NewEventBus()
	bus.Publish("sess", Warning("before", "agent"))

	sub, snapshot := bus.SubscribeWithSnapshot(t.Context(), "sess", 4)
	require.Len(t, snapshot, 1)
	assert.Equal(t, "warning", snapshot[0].(*WarningEvent).Type)

	bus.Publish("sess", Warning("after", "agent"))
	assert.Equal(t, uint64(2), bus.TopicSequence("sess"))
	bus.CloseTopic("sess")

	assert.Equal(t, "after", (<-sub.Events()).(*WarningEvent).Message)
	_, ok := <-sub.Events()
	assert.False(t, ok)
}

func TestEventBusPublishCloseTopicRaceDoesNotPanic(t *testing.T) {
	bus := NewEventBus()
	for range 100 {
		sub := bus.Subscribe(t.Context(), "sess", 1)
		done := make(chan struct{})
		go func() {
			defer close(done)
			for range sub.Events() {
			}
		}()
		publishDone := make(chan struct{})
		go func() {
			defer close(publishDone)
			for range 20 {
				bus.Publish("sess", Warning("event", "agent"))
			}
		}()
		bus.CloseTopic("sess")
		<-publishDone
		<-done
	}
}

func TestSessionQueueEventStableTypeField(t *testing.T) {
	payload, err := json.Marshal(SessionQueue("sess", 1, []string{"hello"}))
	require.NoError(t, err)
	assert.JSONEq(t, `{"type":"session_queue","session_id":"sess","count":1,"previews":["hello"]}`, string(payload))
}

func TestMessageQueueSnapshotDoesNotDrain(t *testing.T) {
	q := NewInMemoryMessageQueue(2)
	require.True(t, q.Enqueue(t.Context(), QueuedMessage{Content: "one"}))
	snap, ok := q.(QueueSnapshotter)
	require.True(t, ok)
	assert.Equal(t, []QueuedMessage{{Content: "one"}}, snap.Snapshot())
	got, ok := q.Dequeue(t.Context())
	require.True(t, ok)
	assert.Equal(t, "one", got.Content)
}

func TestAttachLiveSessionWithSnapshotGapFreeTail(t *testing.T) {
	r := &LocalRuntime{eventBus: NewEventBus(), liveSessions: newLiveSessionRegistry()}
	r.liveSessions.register("sess", "agent", "")
	r.eventBus.Publish("sess", Warning("before", "agent"))

	snapshot, tail, err := r.AttachLiveSessionWithSnapshot(t.Context(), "sess", 4)
	require.NoError(t, err)
	require.Len(t, snapshot, 1)

	r.eventBus.Publish("sess", Warning("after", "agent"))
	select {
	case ev := <-tail:
		assert.Equal(t, "after", ev.(*WarningEvent).Message)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for tail event")
	}
}

func TestLiveSessionRegistryRegisterUnregister(t *testing.T) {
	reg := newLiveSessionRegistry()
	reg.register("sess", "agent", "parent")
	info, ok := reg.get("sess")
	require.True(t, ok)
	assert.Equal(t, liveSessionInfo{SessionID: "sess", AgentName: "agent", ParentID: "parent"}, info)
	reg.unregister("sess")
	_, ok = reg.get("sess")
	assert.False(t, ok)
}

func TestChildQueuesDoNotStealRootMessages(t *testing.T) {
	r := &LocalRuntime{
		steerQueue:    NewInMemoryMessageQueue(defaultSteerQueueCapacity),
		followUpQueue: NewInMemoryMessageQueue(defaultFollowUpQueueCapacity),
		childQueues:   make(map[string]sessionQueues),
	}
	root := session.New(session.WithID("root"))
	child := session.NewRuntimeManagedSubSession(root, session.WithID("child"))

	require.True(t, r.queuesFor(root).followUp.Enqueue(t.Context(), QueuedMessage{Content: "root-followup"}))
	_, ok := r.queuesFor(child).followUp.Dequeue(t.Context())
	assert.False(t, ok, "child must not dequeue root follow-ups")
	got, ok := r.queuesFor(root).followUp.Dequeue(t.Context())
	require.True(t, ok)
	assert.Equal(t, "root-followup", got.Content)

	require.True(t, r.queuesFor(root).steer.Enqueue(t.Context(), QueuedMessage{Content: "root-steer"}))
	_, ok = r.queuesFor(child).steer.Dequeue(t.Context())
	assert.False(t, ok, "child must not dequeue root steers")
	got, ok = r.queuesFor(root).steer.Dequeue(t.Context())
	require.True(t, ok)
	assert.Equal(t, "root-steer", got.Content)
}
