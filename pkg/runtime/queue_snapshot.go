package runtime

type QueueSnapshotter interface {
	Snapshot() []QueuedMessage
}
