package runtime

import (
	"context"

	"github.com/docker/docker-agent/pkg/session"
)

// EventObserver receives the runtime's event stream as it's produced.
// Implementations subscribe to lifecycle moments and act on them —
// persisting to a store, forwarding to a metrics pipeline, writing an
// audit transcript, etc.
//
// Concurrency: the runtime invokes observers synchronously from the
// goroutine that forwards events to the consumer's channel, in
// registration order. A slow observer therefore back-pressures both
// downstream observers and the consumer; long-running work (network
// I/O, file syncing) should fan out to a private goroutine.
//
// Errors: observers do not return errors. The runtime cannot recover
// from a misbehaving observer (it can't unregister it mid-stream and
// can't ask the consumer to retry), so an observer must log internally
// and never panic. The contract is "best-effort observation" rather
// than "all-or-nothing transactional".
//
// Observers see every event the runtime emits, including sub-session
// events (from delegated tasks via transfer_task) and
// [SessionScoped]-mismatch events. Filtering is the observer's
// responsibility; see [SessionRecorder] for the canonical pattern.
type EventObserver interface {
	// OnRunStart fires once when [LocalRuntime.RunStream] begins, before
	// any event is dispatched. Use it for one-shot lifecycle work like
	// persisting initial session metadata.
	OnRunStart(ctx context.Context, sess *session.Session)
	// OnEvent fires once per event, after the runtime emits it but
	// before the consumer's channel receives it. Observers cannot
	// modify or suppress events (a future extension may relax this);
	// to drop an event from persistence, simply ignore it inside
	// OnEvent.
	OnEvent(ctx context.Context, sess *session.Session, event Event)
}

// WithEventObserver appends o to the runtime's observer chain.
// Observers are invoked in registration order, synchronously, on every
// event the runtime produces. Multiple calls are additive.
//
// The runtime auto-registers a [SessionRecorder] for the configured
// session store; users do not need to wire persistence themselves.
// Custom observers (telemetry, audit, metrics, A2A forward) compose
// alongside that one.
func WithEventObserver(o EventObserver) Opt {
	return func(r *LocalRuntime) {
		if o == nil {
			return
		}
		r.observers = append(r.observers, o)
	}
}

// observe wraps inner with runtime fan-out: each event is first published to
// the EventBus (where the SessionRecorder is registered as a global observer),
// then delivered to custom observers and the caller. When the producer closes,
// the recorder is flushed before the public channel closes so callers can
// immediately observe durable state.
func (r *LocalRuntime) observe(ctx context.Context, sess *session.Session, inner <-chan Event) <-chan Event {
	r.ensureSessionPersisted(ctx, sess)
	if r.liveSessions != nil && sess != nil {
		agentName := sess.AgentName
		if agentName == "" {
			agentName = r.CurrentAgentName()
		}
		r.liveSessions.register(sess.ID, agentName, sess.ParentID)
	}
	for _, obs := range r.observers {
		obs.OnRunStart(ctx, sess)
	}
	out := make(chan Event, cap(inner))
	go func() {
		defer close(out)
		for event := range inner {
			if r.eventBus != nil {
				r.eventBus.Publish(sess.ID, event)
			}
			for _, obs := range r.observers {
				obs.OnEvent(ctx, sess, event)
			}
			out <- event
		}
		if r.recorder != nil {
			r.recorder.FlushSession(sess.ID)
		}
		// Runtime-managed child sessions are parked by the subagent manager between
		// turns. Keep their live registry entry and event topic open until the manager
		// reaches an explicit terminal lifecycle path (stop/finalize/root close),
		// otherwise post-turn attach and parent-to-child follow-ups lose the session.
		if !sess.RuntimeManaged {
			if r.liveSessions != nil {
				r.liveSessions.unregister(sess.ID)
			}
			if r.eventBus != nil {
				r.eventBus.CloseTopic(sess.ID)
			}
		}
	}()
	return out
}
