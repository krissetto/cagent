package runtime

import (
	"context"
	"sync"

	"github.com/docker/docker-agent/pkg/chat"
)

// QueuedMessage is a user message waiting to be injected into the agent loop,
// either mid-turn (via the steer queue) or at end-of-turn (via the follow-up
// queue).
type QueuedMessage struct {
	Content      string
	MultiContent []chat.MessagePart
}

// MessageQueue is the interface for storing messages that are injected into
// the agent loop. Implementations must be safe for concurrent use: Enqueue
// is called from API handlers while Dequeue/Drain are called from the agent
// loop goroutine.
//
// The default implementation is NewInMemoryMessageQueue. Callers that need
// durable or distributed storage can provide their own implementation
// via the WithSteerQueue or WithFollowUpQueue options.
type MessageQueue interface {
	// Enqueue adds a message to the queue. Returns false if the queue is
	// full or the context is cancelled.
	Enqueue(ctx context.Context, msg QueuedMessage) bool
	// Dequeue removes and returns the next message from the queue.
	// Returns the message and true, or a zero value and false if the
	// queue is empty. Must not block.
	Dequeue(ctx context.Context) (QueuedMessage, bool)
	// Drain returns all pending messages and removes them from the queue.
	// Must not block — if the queue is empty it returns nil.
	Drain(ctx context.Context) []QueuedMessage
}

// inMemoryMessageQueue is the default MessageQueue backed by a mutex-protected
// slice. It implements a coalesced Signal channel for callers that can use a
// fast wake-up path without consuming queue contents.
type inMemoryMessageQueue struct {
	mu       sync.Mutex
	messages []QueuedMessage
	capacity int
	signal   chan struct{}
}

const (
	// defaultSteerQueueCapacity is the buffer size for the default in-memory steer queue.
	defaultSteerQueueCapacity = 5
	// defaultFollowUpQueueCapacity is the buffer size for the default in-memory follow-up queue.
	// Higher than steer because follow-ups accumulate while waiting for the turn to end.
	defaultFollowUpQueueCapacity = 20
)

// NewInMemoryMessageQueue creates a MessageQueue backed by a buffered channel
// with the given capacity.
func NewInMemoryMessageQueue(capacity int) MessageQueue {
	return &inMemoryMessageQueue{
		capacity: capacity,
		signal:   make(chan struct{}, 1),
	}
}

func (q *inMemoryMessageQueue) Enqueue(_ context.Context, msg QueuedMessage) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.messages) >= q.capacity {
		return false
	}
	q.messages = append(q.messages, msg)
	q.notify()
	return true
}

func (q *inMemoryMessageQueue) Dequeue(_ context.Context) (QueuedMessage, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.clearSignal()
	if len(q.messages) == 0 {
		return QueuedMessage{}, false
	}
	msg := q.messages[0]
	copy(q.messages, q.messages[1:])
	q.messages = q.messages[:len(q.messages)-1]
	return msg, true
}

func (q *inMemoryMessageQueue) Drain(_ context.Context) []QueuedMessage {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.clearSignal()
	msgs := append([]QueuedMessage(nil), q.messages...)
	q.messages = nil
	return msgs
}

func (q *inMemoryMessageQueue) Snapshot() []QueuedMessage {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]QueuedMessage(nil), q.messages...)
}

func (q *inMemoryMessageQueue) Signal() <-chan struct{} { return q.signal }

func (q *inMemoryMessageQueue) notify() {
	select {
	case q.signal <- struct{}{}:
	default:
	}
}

func (q *inMemoryMessageQueue) clearSignal() {
	select {
	case <-q.signal:
	default:
	}
}

// QueueStatus represents the current depth and capacity of message queues
type QueueStatus struct {
	SteerDepth       int
	SteerCapacity    int
	FollowupDepth    int
	FollowupCapacity int
}
