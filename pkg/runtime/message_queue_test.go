package runtime

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInMemoryMessageQueue_Signal(t *testing.T) {
	t.Parallel()

	q := NewInMemoryMessageQueue(4)
	signal := q.Signal()

	require.True(t, q.Enqueue(t.Context(), QueuedMessage{Content: "hello"}))

	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatal("expected queue signal after enqueue")
	}

	msg, ok := q.Dequeue(t.Context())
	require.True(t, ok)
	assert.Equal(t, "hello", msg.Content)
}
