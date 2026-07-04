package runtime

import (
	"context"

	"github.com/docker/docker-agent/pkg/session"
)

// The session actor: the runtime can drive any session it has seen, so a
// note arriving while a session is idle starts a runtime-owned run instead
// of depending on the embedder to relay it. Sessions fall into two camps:
//
//   - receiver-managed: an interactive embedder (the TUI) registered a
//     MessageReceiver at least once. The embedder owns interactivity (tool
//     approvals, elicitations), so the runtime never runs these unattended —
//     during receiver gaps notes buffer and drain when the receiver returns.
//
//   - actor-managed: everything else (API server, protocol adapters, exec).
//     A note to an idle session wakes it: a runtime-owned run whose opening
//     steer drain turns the note into a regular steered user message, with
//     the standard hooks, events, and persistence (the persistence observer
//     is part of every RunStream). Events reach embedders through the
//     session event hub, the same "subscribe to the session's event stream"
//     they already use for live viewing.
//
// Embedder-initiated turns go through [LocalRuntime.RunOrAttach], which
// keeps the classic shape (stage the message, run, stream the turn) when the
// session is free and attaches to the live run when the actor got there
// first. The single-driver invariant holds throughout: one loop per session,
// everyone else steers into it or watches the hub.

// rememberSession records the session object so the actor can wake it later.
// The pointer is shared with the embedder on purpose: a wake mutates the
// same session every other holder sees.
func (r *LocalRuntime) rememberSession(sess *session.Session) {
	r.sessionRunsMu.Lock()
	defer r.sessionRunsMu.Unlock()
	if r.knownSessions == nil {
		r.knownSessions = map[string]*session.Session{}
	}
	r.knownSessions[sess.ID] = sess
}

// isReceiverManaged reports whether a session's embedder ever registered a
// receiver, marking it interactive (see the package note above).
func (r *LocalRuntime) isReceiverManaged(sessionID string) bool {
	r.receiversMu.RLock()
	defer r.receiversMu.RUnlock()
	return r.receiverManaged[sessionID]
}

// wakeSession starts a runtime-owned run for an idle actor-managed session
// with buffered input. No-op when the session is unknown, already running,
// reserved or waking, or has nothing buffered — every caller may fire it
// opportunistically.
func (r *LocalRuntime) wakeSession(sessionID string) {
	r.sessionRunsMu.Lock()
	sess := r.knownSessions[sessionID]
	if sess == nil || r.sessionWaking[sessionID] || r.sessionReserved[sessionID] ||
		r.sessionRuns[sessionID] > 0 || len(r.sessionSteer[sessionID]) == 0 {
		r.sessionRunsMu.Unlock()
		return
	}
	r.sessionWaking[sessionID] = true
	r.sessionRunsMu.Unlock()

	// The wake outlives whatever delivery triggered it: it runs on the
	// runtime's root context, like the subagent manager's child runs.
	ctx := context.WithoutCancel(r.ctx())
	go r.runWakeLoop(ctx, sess)
}

// runWakeLoop drives wake runs until the session's buffer stays empty. The
// loop's opening steer drain consumes the buffered notes; notes racing in
// after a run's final drain just trigger another lap, exactly like the
// subagent manager's child turn loop. A driver reservation (an embedder
// turn waiting in attachThenRun) ends the loop — the reserved run's opening
// drain takes over whatever is left in the buffer.
func (r *LocalRuntime) runWakeLoop(ctx context.Context, sess *session.Session) {
	for {
		for range r.RunStream(ctx, sess) {
			// Drained for flow control only: observers persist and the
			// session event hub mirrors to every subscriber.
		}
		r.sessionRunsMu.Lock()
		if len(r.sessionSteer[sess.ID]) == 0 || r.sessionReserved[sess.ID] {
			delete(r.sessionWaking, sess.ID)
			r.sessionRunsMu.Unlock()
			return
		}
		r.sessionRunsMu.Unlock()
	}
}

// RunOrAttach is the actor-aware replacement for calling RunStream after
// staging user messages on the session:
//
//   - free session: exactly RunStream, byte-for-byte — the returned channel
//     is the run's own stream, closing when the run stops. The session is
//     reserved first so a note-triggered wake cannot race in as a second
//     driver.
//   - runtime-owned run in flight (an actor wake): no second driver starts.
//     The staged messages are already on the session, so the live run picks
//     them up at its next model call, and the returned channel mirrors the
//     run from the session event hub (seeded mid-stream), closing once the
//     session settles.
//
// Either way the caller's contract is unchanged from RunStream: one channel
// that carries the turn and then closes.
func (r *LocalRuntime) RunOrAttach(ctx context.Context, sess *session.Session) <-chan Event {
	r.sessionRunsMu.Lock()
	if r.knownSessions == nil {
		r.knownSessions = map[string]*session.Session{}
	}
	if r.sessionReserved == nil {
		r.sessionReserved = map[string]bool{}
	}
	r.knownSessions[sess.ID] = sess
	drives := r.sessionRuns[sess.ID] == 0 && !r.sessionWaking[sess.ID] && !r.sessionReserved[sess.ID]
	if drives {
		// Driver reservation, consumed by the run's beginSessionRun.
		r.sessionReserved[sess.ID] = true
	}
	r.sessionRunsMu.Unlock()

	if drives {
		return r.RunStream(ctx, sess)
	}
	return r.attachThenRun(ctx, sess)
}

// attachThenRun mirrors the live runtime-owned run from the hub and, once
// the session settles, becomes the driver and runs the caller's staged turn.
// The staged messages may already be answered by the live run (it re-reads
// the session each model call); the follow-up run then simply completes
// whatever is left — buffered notes included. Cancelling ctx detaches the
// mirror and cancels only the caller's own run, never the runtime-owned one.
func (r *LocalRuntime) attachThenRun(ctx context.Context, sess *session.Session) <-chan Event {
	out := make(chan Event, defaultEventChannelCapacity)
	go func() {
		defer close(out)
		emit := func(e Event) bool {
			select {
			case out <- e:
				return true
			case <-ctx.Done():
				return false
			}
		}
		drive := func() {
			run := r.RunStream(ctx, sess)
			for e := range run {
				if !emit(e) {
					// Keep draining so the run's goroutine can finish.
					for range run {
					}
					return
				}
			}
		}

		seed, events, cancel := r.SubscribeSessionEvents(sess.ID)
		defer cancel()
		if r.reserveIfSettled(sess.ID) {
			// The live run ended between the caller's check and our
			// subscription; we are the driver after all.
			cancel()
			drive()
			return
		}
		for _, e := range seed {
			if !emit(e) {
				return
			}
		}
		for {
			select {
			case <-ctx.Done():
				return
			case e, ok := <-events:
				if !ok || !emit(e) {
					return
				}
				if _, stopped := e.(*StreamStoppedEvent); stopped && r.reserveIfSettled(sess.ID) {
					cancel()
					drive()
					return
				}
			}
		}
	}()
	return out
}

// reserveIfSettled atomically takes the driver reservation when the session
// has no live run, waker, or prior reservation. A non-empty steer buffer
// does not block it: the reserved run's opening drain consumes the buffer.
func (r *LocalRuntime) reserveIfSettled(sessionID string) bool {
	r.sessionRunsMu.Lock()
	defer r.sessionRunsMu.Unlock()
	if r.sessionRuns[sessionID] > 0 || r.sessionWaking[sessionID] || r.sessionReserved[sessionID] {
		return false
	}
	if r.sessionReserved == nil {
		r.sessionReserved = map[string]bool{}
	}
	r.sessionReserved[sessionID] = true
	return true
}

// sessionSettled reports that a session has no live or pending work: no
// running loop, no waker or reservation, nothing buffered.
func (r *LocalRuntime) sessionSettled(sessionID string) bool {
	r.sessionRunsMu.Lock()
	defer r.sessionRunsMu.Unlock()
	return r.sessionRuns[sessionID] == 0 && !r.sessionWaking[sessionID] &&
		!r.sessionReserved[sessionID] && len(r.sessionSteer[sessionID]) == 0
}

// SessionRunner is the capability embedders assert to route turns through
// the session actor instead of raw RunStream. Runtimes that don't implement
// it (remote) keep the classic RunStream path.
type SessionRunner interface {
	RunOrAttach(ctx context.Context, sess *session.Session) <-chan Event
}

// RunOrAttachStream routes a staged session turn through the runtime's
// session actor when the runtime hosts one, falling back to plain RunStream
// (remote runtimes). Same contract either way: one channel, the whole turn,
// then closed. This is the drop-in serve-surface replacement for calling
// RunStream after staging user messages.
func RunOrAttachStream(ctx context.Context, rt Runtime, sess *session.Session) <-chan Event {
	if sr, ok := rt.(SessionRunner); ok {
		return sr.RunOrAttach(ctx, sess)
	}
	return rt.RunStream(ctx, sess)
}
