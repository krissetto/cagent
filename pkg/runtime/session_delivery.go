package runtime

import (
	"context"
)

// Detached message delivery: how runtime-authored messages (notes from async
// subagents) and external callers reach a session that may or may not have a
// live run loop. A live loop receives them as session-scoped steering at its
// normal iteration boundaries; an idle session is woken through its
// registered MessageReceiver.

// MessageReceiver hands an incoming message to a session's owner, who is
// responsible for starting a fresh runtime loop if the session is idle. The
// TUI/app implementation routes through its normal SendMsg path so busy
// sessions queue and idle sessions run immediately.
type MessageReceiver func(ctx context.Context, content string)

// RegisterMessageReceiver registers the delivery path for detached runtime
// messages targeting a session, such as notes from async subagents. The
// returned function unregisters it.
//
// Registering also drains any notes buffered while the session had no
// receiver (e.g. the owning tab was switched away): the wake path just
// (re)appeared, so they are delivered through it immediately instead of
// waiting for the session's next run. Notes buffered under a live run are
// left alone — they belong to that loop's steer drains.
func (r *LocalRuntime) RegisterMessageReceiver(sessionID string, fn MessageReceiver) func() {
	r.receiversMu.Lock()
	if r.receivers == nil {
		r.receivers = map[string]MessageReceiver{}
	}
	r.receivers[sessionID] = fn
	r.receiversMu.Unlock()

	r.sessionRunsMu.Lock()
	var pending []QueuedMessage
	if r.sessionRuns[sessionID] == 0 {
		pending = r.sessionSteer[sessionID]
		delete(r.sessionSteer, sessionID)
	}
	r.sessionRunsMu.Unlock()
	if len(pending) > 0 {
		// Not the registration's call stack on purpose: the receiver may
		// synchronously start a run, and callers register during setup. The
		// runtime's root context scopes the handover, like any detached
		// delivery.
		go func() {
			for _, sm := range pending {
				fn(context.WithoutCancel(r.ctx()), sm.Content)
			}
		}()
	}

	return func() {
		r.receiversMu.Lock()
		if r.receivers[sessionID] == nil {
			r.receiversMu.Unlock()
			return
		}
		delete(r.receivers, sessionID)
		r.receiversMu.Unlock()
	}
}

// HasMessageReceiver reports whether a session currently has a wake path for
// detached messages. Async subagents require one on the spawning session:
// without it, turn reports could only reach the parent on its next
// embedder-initiated run, so spawning is refused up front instead of
// degrading to that.
func (r *LocalRuntime) HasMessageReceiver(sessionID string) bool {
	r.receiversMu.RLock()
	defer r.receiversMu.RUnlock()
	return r.receivers[sessionID] != nil
}

func (r *LocalRuntime) deliverMessage(ctx context.Context, sessionID, content string) bool {
	// Earliest safe delivery: when the session's loop is live, buffer the
	// message in its session-scoped steer queue. The loop drains it at the
	// next iteration boundary (after the in-flight batch of tool calls, and
	// again right before the stop check), exactly like user steering. Only an
	// idle session falls through to its receiver, which starts a fresh run.
	r.sessionRunsMu.Lock()
	if r.sessionRuns[sessionID] > 0 {
		r.bufferSessionSteerLocked(sessionID, content)
		r.sessionRunsMu.Unlock()
		return true
	}
	r.sessionRunsMu.Unlock()

	r.receiversMu.RLock()
	fn := r.receivers[sessionID]
	r.receiversMu.RUnlock()
	if fn == nil {
		return false
	}
	fn(ctx, content)
	return true
}

// deliverOrBuffer is deliverMessage for messages that must never be lost:
// when the session is idle and has no receiver, the message stays in the
// steer buffer until a wake path reappears — the next run's opening steer
// drain, or a receiver re-registration (RegisterMessageReceiver drains it).
//
// This is the right semantic for parent-directed notes (turn reports,
// send_message(parent)): spawning requires a receiver on the parent, so a
// missing one here is a transient gap (owning tab switched away, teardown
// race), not an unsupported host. Child-directed input must use
// deliverMessage instead: a child without a receiver is stopped and will
// never run again, so buffering would silently lose the message — failing is
// truthful there.
func (r *LocalRuntime) deliverOrBuffer(ctx context.Context, sessionID, content string) {
	if r.deliverMessage(ctx, sessionID, content) {
		return
	}
	r.sessionRunsMu.Lock()
	defer r.sessionRunsMu.Unlock()
	r.bufferSessionSteerLocked(sessionID, content)
}

// bufferSessionSteerLocked appends a message to the session's steer buffer.
// Caller holds sessionRunsMu.
func (r *LocalRuntime) bufferSessionSteerLocked(sessionID, content string) {
	if r.sessionSteer == nil {
		r.sessionSteer = map[string][]QueuedMessage{}
	}
	r.sessionSteer[sessionID] = append(r.sessionSteer[sessionID], QueuedMessage{Content: content})
}

// beginSessionRun marks a live run-stream loop for the session so deliverMessage
// steers into it instead of starting a competing run.
func (r *LocalRuntime) beginSessionRun(sessionID string) {
	r.sessionRunsMu.Lock()
	defer r.sessionRunsMu.Unlock()
	if r.sessionRuns == nil {
		r.sessionRuns = map[string]int{}
	}
	r.sessionRuns[sessionID]++
}

// endSessionRun unmarks the loop. Messages that raced with teardown (buffered
// after the loop's final steer drain) are re-dispatched so a registered
// receiver starts a fresh run; without one they return to the buffer and wait
// for the next run instead of stranding unnoticed.
func (r *LocalRuntime) endSessionRun(ctx context.Context, sessionID string) {
	r.sessionRunsMu.Lock()
	r.sessionRuns[sessionID]--
	if r.sessionRuns[sessionID] > 0 {
		r.sessionRunsMu.Unlock()
		return
	}
	delete(r.sessionRuns, sessionID)
	stranded := r.sessionSteer[sessionID]
	delete(r.sessionSteer, sessionID)
	r.sessionRunsMu.Unlock()

	if len(stranded) == 0 {
		return
	}
	// Detached from the dying loop's lifetime on purpose: delivery must
	// survive the loop's cancellation, and the receiver may synchronously
	// start a new run.
	ctx = context.WithoutCancel(ctx)
	go func() {
		for _, sm := range stranded {
			r.deliverOrBuffer(ctx, sessionID, sm.Content)
		}
	}()
}

// drainSessionSteer takes all buffered detached messages for a session.
func (r *LocalRuntime) drainSessionSteer(sessionID string) []QueuedMessage {
	r.sessionRunsMu.Lock()
	defer r.sessionRunsMu.Unlock()
	msgs := r.sessionSteer[sessionID]
	delete(r.sessionSteer, sessionID)
	return msgs
}

// SubscribeSessionEvents attaches a viewer to a session's run events: every
// event any run of that session emits (streaming deltas, tool calls, steered
// user messages, stream lifecycle) is mirrored to the returned channel. seed
// carries the in-flight assistant message streamed so far (as synthetic delta
// events), captured atomically with the subscription — a viewer attaching
// mid-stream renders the message's head from seed and its tail from the
// channel, exactly once. The session's driver is unaffected; slow viewers
// drop events. Cancel closes the channel.
func (r *LocalRuntime) SubscribeSessionEvents(sessionID string) (seed []Event, events <-chan Event, cancel func()) {
	return r.sessionEvents.Subscribe(sessionID, defaultEventChannelCapacity)
}

// DeliverMessage delivers a detached user message to a session by id: steered
// into its live run loop when one is running, otherwise handed to its
// registered receiver (for an async subagent's sub-session, the manager
// re-runs the subagent). Returns false when the session is idle and has no
// receiver.
func (r *LocalRuntime) DeliverMessage(ctx context.Context, sessionID, content string) bool {
	return r.deliverMessage(ctx, sessionID, content)
}
