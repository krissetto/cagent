package runtime

import (
	"strings"

	"github.com/docker/docker-agent/pkg/session"
)

type sessionQueues struct {
	steer    MessageQueue
	followUp MessageQueue
}

func (r *LocalRuntime) queuesFor(sess *session.Session) sessionQueues {
	if sess == nil || sess.ParentID == "" {
		return sessionQueues{steer: r.steerQueue, followUp: r.followUpQueue}
	}
	return r.childQueuesFor(sess.ID)
}

func (r *LocalRuntime) childQueuesFor(sessionID string) sessionQueues {
	if r == nil || sessionID == "" {
		return sessionQueues{steer: NewInMemoryMessageQueue(defaultSteerQueueCapacity), followUp: NewInMemoryMessageQueue(defaultFollowUpQueueCapacity)}
	}
	r.queuesMu.Lock()
	defer r.queuesMu.Unlock()
	queues := r.childQueues[sessionID]
	if queues.steer == nil {
		queues = sessionQueues{
			steer:    NewInMemoryMessageQueue(defaultSteerQueueCapacity),
			followUp: NewInMemoryMessageQueue(defaultFollowUpQueueCapacity),
		}
		r.childQueues[sessionID] = queues
	}
	return queues
}

func (r *LocalRuntime) publishQueueSnapshot(sessionID string, queue MessageQueue) {
	if r == nil || r.eventBus == nil || sessionID == "" || queue == nil {
		return
	}
	pending, ok := queue.(QueueSnapshotter)
	if !ok {
		return
	}
	messages := pending.Snapshot()
	previews := make([]string, 0, len(messages))
	for _, msg := range messages {
		preview := strings.TrimSpace(msg.Content)
		if idx := strings.IndexByte(preview, '\n'); idx >= 0 {
			preview = preview[:idx]
		}
		previews = append(previews, preview)
	}
	r.eventBus.Publish(sessionID, SessionQueue(sessionID, len(messages), previews))
}
