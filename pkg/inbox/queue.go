package inbox

import "sync"

// Queue is a concurrent FIFO queue with non-blocking enqueue, single-item
// dequeue, drain-all, close, and coalesced notification semantics.
//
// A zero or negative capacity means unbounded. A positive capacity means
// Push returns false when the queue already holds capacity items.
//
// Queue deliberately uses the same [Signal] stale-tick hygiene as the
// subagent parent envelope inbox: every successful Push notifies, while
// Dequeue and Drain consume any buffered notification in the same critical
// section that observes/removes items.
type Queue[T any] struct {
	mu       sync.Mutex
	items    []T
	capacity int
	notify   Signal
	closed   bool
}

// NewQueue creates a bounded queue when capacity > 0, otherwise an
// unbounded queue.
func NewQueue[T any](capacity int) *Queue[T] {
	return &Queue[T]{capacity: capacity, notify: NewSignal()}
}

// NewUnboundedQueue creates a queue with no item limit.
func NewUnboundedQueue[T any]() *Queue[T] { return NewQueue[T](0) }

// Push enqueues item and wakes waiters. It returns false when the queue is
// closed or when a bounded queue is full.
//
// Notify is called while still holding the lock so that a concurrent Drain
// or Dequeue cannot consume the item and then see a stale tick afterwards.
func (q *Queue[T]) Push(item T) bool {
	q.mu.Lock()
	if q.closed || (q.capacity > 0 && len(q.items) >= q.capacity) {
		q.mu.Unlock()
		return false
	}
	q.items = append(q.items, item)
	q.notify.Notify()
	q.mu.Unlock()
	return true
}

// Dequeue removes and returns the next item without blocking. It consumes
// any pending signal tick because the caller is observing the queue now.
func (q *Queue[T]) Dequeue() (T, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.notify.Consume()
	if len(q.items) == 0 {
		var zero T
		return zero, false
	}
	item := q.items[0]
	copy(q.items, q.items[1:])
	var zero T
	q.items[len(q.items)-1] = zero
	q.items = q.items[:len(q.items)-1]
	return item, true
}

// Drain removes and returns every queued item without blocking. It consumes
// any pending signal tick because the caller is observing the queue now.
func (q *Queue[T]) Drain() []T {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.notify.Consume()
	if len(q.items) == 0 {
		return nil
	}
	items := q.items
	q.items = nil
	return items
}

// Signal returns the coalesced notification channel. Receivers must always
// re-check queue state after waking.
func (q *Queue[T]) Signal() <-chan struct{} { return q.notify.C() }

// HasItems reports whether at least one item is queued.
func (q *Queue[T]) HasItems() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items) > 0
}

// Close prevents future Push calls and wakes any waiter.
func (q *Queue[T]) Close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.notify.Notify()
}
