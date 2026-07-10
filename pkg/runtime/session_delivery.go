package runtime

import (
	"context"

	"github.com/docker/docker-agent/pkg/session"
)

// Detached message delivery is owned by per-session drivers: a live session
// buffers input for its next steering drain; an idle known session can be
// woken by runtime-authored notes; unknown sessions keep wakeable notes until
// their session object is seen.

func (r *LocalRuntime) deliverMessage(ctx context.Context, sessionID, content string) bool {
	return r.sessionDrivers.PostKnown(ctx, sessionID, QueuedMessage{Content: content}, false)
}

func (r *LocalRuntime) deliverOrBuffer(ctx context.Context, sessionID, content string) {
	_ = r.sessionDrivers.PostOrBuffer(ctx, sessionID, QueuedMessage{Content: content}, true)
}

func (r *LocalRuntime) drainSessionSteer(sessionID string) []QueuedMessage {
	return r.sessionDrivers.Drain(sessionID)
}

func (r *LocalRuntime) SubscribeSessionEvents(sessionID string) (seed []Event, events <-chan Event, cancel func()) {
	if d, ok := r.sessionDrivers.Lookup(sessionID); ok {
		return d.Subscribe(defaultEventChannelCapacity)
	}
	return r.sessionEvents.Subscribe(sessionID, defaultEventChannelCapacity)
}

// DeliverMessage delivers detached input to a known session by id, waking an
// idle driver when needed. It returns false when the runtime has never seen the
// session; callers that must never lose a note use deliverOrBuffer instead.
func (r *LocalRuntime) DeliverMessage(ctx context.Context, sessionID, content string) bool {
	return r.sessionDrivers.PostKnown(ctx, sessionID, QueuedMessage{Content: content}, true)
}

func (r *LocalRuntime) rememberSession(sess *session.Session) {
	r.sessionDrivers.Get(sess)
}
