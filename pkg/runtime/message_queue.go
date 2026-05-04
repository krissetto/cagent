package runtime

import (
	"context"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/inbox"
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
	// Signal returns a receive-only channel that is notified whenever a
	// message is enqueued. The channel has a buffer of 1 so a slow reader
	// can miss intermediate enqueues, but a fresh receive is guaranteed
	// for any enqueue that happens after the last drain. Callers should
	// therefore treat a signal as "something might be available" and
	// re-check via Dequeue/Drain.
	Signal() <-chan struct{}
}

// inMemoryMessageQueue is the default MessageQueue adapter over the shared
// inbox.Queue primitive.
type inMemoryMessageQueue struct {
	q *inbox.Queue[QueuedMessage]
}

const (
	// defaultSteerQueueCapacity is the buffer size for the default in-memory steer queue.
	defaultSteerQueueCapacity = 5
	// defaultFollowUpQueueCapacity is the buffer size for the default in-memory follow-up queue.
	// Higher than steer because follow-ups accumulate while waiting for the turn to end.
	defaultFollowUpQueueCapacity = 20
)

// NewInMemoryMessageQueue creates a MessageQueue backed by a shared inbox.Queue
// with the given capacity.
func NewInMemoryMessageQueue(capacity int) MessageQueue {
	return &inMemoryMessageQueue{q: inbox.NewQueue[QueuedMessage](capacity)}
}

func (q *inMemoryMessageQueue) Enqueue(_ context.Context, msg QueuedMessage) bool {
	return q.q.Push(msg)
}

func (q *inMemoryMessageQueue) Dequeue(_ context.Context) (QueuedMessage, bool) {
	return q.q.Dequeue()
}

func (q *inMemoryMessageQueue) Drain(_ context.Context) []QueuedMessage {
	return q.q.Drain()
}

func (q *inMemoryMessageQueue) Signal() <-chan struct{} {
	return q.q.Signal()
}
