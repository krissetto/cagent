package runtime

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
)

func TestWaitForSubagentWorkDoesNotDrainQueuedParentInput(t *testing.T) {
	root := session.New(session.WithID("root"))
	r := &LocalRuntime{steerQueue: NewInMemoryMessageQueue(4), followUpQueue: NewInMemoryMessageQueue(4)}
	m := NewSubagentManager(r)
	child := session.NewRuntimeManagedSubSession(root, session.WithID("child"))
	h := &subagentHandle{id: child.ID, shortID: "child", parent: root, sess: child, done: make(chan struct{}), wake: make(chan struct{}, 1)}
	m.all[h.id] = h
	require.True(t, r.followUpQueue.Enqueue(t.Context(), QueuedMessage{Content: "preserve followup"}))

	resumed := m.WaitForSubagentWork(t.Context(), root, r.queuesFor(root), EventSinkFunc(func(Event) {}))
	require.True(t, resumed)
	got, ok := r.followUpQueue.Dequeue(t.Context())
	require.True(t, ok)
	assert.Equal(t, "preserve followup", got.Content)
}

func TestSubagentDepthConcurrentAccessRaceRegression(t *testing.T) {
	root := session.New(session.WithID("root"))
	r := &LocalRuntime{now: time.Now}
	m := NewSubagentManager(r)
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			child := session.NewRuntimeManagedSubSession(root)
			h := &subagentHandle{id: child.ID, shortID: shortID(child.ID), parent: root, sess: child, done: make(chan struct{})}
			m.mu.Lock()
			m.all[h.id] = h
			m.mu.Unlock()
		})
		wg.Go(func() {
			_ = m.depth(root)
		})
	}
	wg.Wait()
}

var _ context.Context
