package inbox

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSignalNotifyAndConsume(t *testing.T) {
	t.Parallel()
	s := NewSignal()

	select {
	case <-s.C():
		t.Fatal("fresh signal should not have a buffered tick")
	default:
	}

	s.Notify()
	s.Notify() // coalesces into a single tick
	select {
	case <-s.C():
	default:
		t.Fatal("notify should produce a tick")
	}

	s.Notify()
	s.Consume()
	select {
	case <-s.C():
		t.Fatal("Consume should drop the buffered tick")
	default:
	}
}

func TestQueueBoundedPushDequeue(t *testing.T) {
	t.Parallel()
	q := NewQueue[int](2)

	require.True(t, q.Push(1))
	require.True(t, q.Push(2))
	require.False(t, q.Push(3), "third push must be rejected on a 2-capacity queue")

	v, ok := q.Dequeue()
	require.True(t, ok)
	assert.Equal(t, 1, v)

	v, ok = q.Dequeue()
	require.True(t, ok)
	assert.Equal(t, 2, v)

	_, ok = q.Dequeue()
	assert.False(t, ok)

	require.True(t, q.Push(4))
	assert.True(t, q.HasItems())
}

func TestQueueDrainOrderingAndConsume(t *testing.T) {
	t.Parallel()
	q := NewUnboundedQueue[string]()

	q.Push("a")
	q.Push("b")
	q.Push("c")

	// A signal tick should be ready before drain.
	select {
	case <-q.Signal():
	default:
		t.Fatal("expected a buffered signal tick after pushes")
	}
	// Drain should still empty the queue (the tick was already consumed
	// above; this verifies Drain itself is consume-safe).
	q.Push("d")
	got := q.Drain()
	assert.Equal(t, []string{"a", "b", "c", "d"}, got)

	// After draining and consuming, no spurious tick should fire.
	select {
	case <-q.Signal():
		t.Fatal("Drain must consume the pending signal tick")
	default:
	}
}

func TestQueueClosePreventsPush(t *testing.T) {
	t.Parallel()
	q := NewQueue[int](0)
	require.True(t, q.Push(1))
	q.Close()
	assert.False(t, q.Push(2), "push on closed queue must be rejected")

	// Close wakes any waiter.
	select {
	case <-q.Signal():
	default:
		t.Fatal("Close should fire a signal tick to wake waiters")
	}

	// Dequeue still drains pre-existing items.
	v, ok := q.Dequeue()
	require.True(t, ok)
	assert.Equal(t, 1, v)
}

func TestQueueConcurrentPushDrain(t *testing.T) {
	t.Parallel()
	q := NewUnboundedQueue[int]()

	const producers, perProducer = 8, 200
	var wg sync.WaitGroup
	wg.Add(producers)
	for i := range producers {
		go func(base int) {
			defer wg.Done()
			for j := range perProducer {
				q.Push(base*perProducer + j)
			}
		}(i)
	}
	wg.Wait()

	got := q.Drain()
	assert.Len(t, got, producers*perProducer)
}
