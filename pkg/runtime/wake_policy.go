// wake_policy.go contains the per-mode wake policies that decide what to do
// after the engine completes a turn: root sessions consume follow-ups and
// keep alive while subagents are in flight; child sessions publish a turn
// envelope and wait on parent/descendant inbox traffic.
package runtime

import (
	"context"
	"log/slog"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

// wakePolicy is the only point of variation between the local in-process
// outer loop and any future runner that wraps the same engine.
//
// Implementations decide what should happen *after* the model emits a
// stopped assistant turn. For root sessions this is "drain the follow-up
// queue, otherwise wait for subagent inbox traffic, otherwise terminate".
// For child sessions it is "publish the turn envelope, wait on the parent
// inbox / descendant signal / close request".
type wakePolicy interface {
	// turnCtx wraps the engine's context for the upcoming turn. Root
	// sessions return ctx unchanged; child sessions derive a per-turn
	// cancel so users can interrupt a single turn without killing the
	// subagent. The engine calls the returned cancel func after the turn
	// body returns.
	turnCtx(parent context.Context) (context.Context, context.CancelFunc)

	// wakeNext is called once the per-turn body returns
	// [outcomeStopped]. Implementations may block until new work arrives
	// (follow-up, parent message, descendant envelope, ...) and inject
	// whatever session mutations are needed before returning.
	//
	// e is the engine that just completed a turn; policies use it to
	// access engine-owned per-session state (inbox, resumeChan, etc.) so
	// they don't need to reach into the runtime's shared state.
	//
	// When info.canceled is true, the previous turn was interrupted by a
	// per-turn context cancellation (e.g. user pressed ESC on an
	// attached tab) while the outer engine context is still alive.
	// Implementations should treat this as a "soft stop" rather than a
	// successful turn.
	//
	// Returning true means the engine should run another turn.
	// Returning false means the engine should exit cleanly; the
	// surrounding RunStream / StartChildLoop goroutine takes care of
	// the final terminal event emission.
	wakeNext(ctx context.Context, e *sessionEngine, info turnInfo, events chan Event) bool

	// drainMidTurn is called at the same safe point as the steer-message
	// and subagent-envelope drains inside [runOneTurn]. Implementations
	// may inject additional messages into the session at this point.
	//
	// Returns true when at least one message was injected so the engine
	// can compact and continue. Root sessions return false (they have
	// dedicated steer/envelope drains). Child sessions drain their direct
	// inbox here so steer-mode messages reach the child mid-turn.
	drainMidTurn(sess *session.Session, events chan Event) bool
}

// rootWakePolicy is the wakePolicy used by [LocalRuntime.RunStream] when a
// session is the root of a runtime tree. It mirrors the historical
// post-stop logic: pop a follow-up if one is queued, otherwise keep the
// session alive while subagents are still in flight, otherwise terminate.
type rootWakePolicy struct {
	runner *sessionRunner
}

func (p rootWakePolicy) turnCtx(parent context.Context) (context.Context, context.CancelFunc) {
	return parent, func() {} // root sessions don't need per-turn cancellation
}

// drainMidTurn is a no-op for root sessions: their mid-turn drains for
// steer messages and subagent envelopes are already wired directly into
// [sessionEngine.runOneTurn].
func (p rootWakePolicy) drainMidTurn(_ *session.Session, _ chan Event) bool {
	return false
}

func (p rootWakePolicy) wakeNext(ctx context.Context, e *sessionEngine, info turnInfo, events chan Event) bool {
	r := p.runner.root
	sess := info.sess
	a := info.agent

	slog.Debug("Conversation stopped", "agent", a.Name())
	r.executeStopHooks(ctx, sess, a, info.responseContent, events)

	// --- FOLLOW-UP: end-of-turn injection ---
	if followUp, ok := e.runner.state.followUp.Dequeue(ctx); ok {
		injectUserMessage(sess, followUp.Content, followUp.MultiContent, func(ev Event) { events <- ev })
		m, _ := p.runner.core.modelsStore.GetModel(ctx, a.Model().ID())
		r.compactIfNeeded(ctx, sess, a, m, info.contextLimit, info.messageCountBeforeTools, events)
		return true
	}

	if p.runner.waitForSubagentInbox(ctx, sess, events) {
		m, _ := p.runner.core.modelsStore.GetModel(ctx, a.Model().ID())
		r.compactIfNeeded(ctx, sess, a, m, info.contextLimit, info.messageCountBeforeTools, events)
		return true
	}

	return false
}

// childWakePolicy is the wakePolicy used by subagent child sessions. After
// each model turn it publishes an envelope to the parent, then waits for one
// of: a new parent→child message, a descendant envelope, a close request, or
// context cancellation.
//
// Per-turn observability comes from [TurnStartedEvent]/[TurnEndedEvent]
// emitted by [sessionEngine.run]; childWakePolicy itself does not emit
// stream events. [StreamStartedEvent]/[StreamStoppedEvent] mark the full
// child session lifetime and are emitted exactly once by
// [sessionRunner.runStreamWithConfig] and [sessionRunner.finalizeEventChannel].
//
// Terminal envelope dispatch (PublishStopped/Failed/Closed) is not part of
// this policy: it lives in [publishTerminalEnvelope] inside StartChildLoop,
// which owns the goroutine lifetime and is the right place to classify engine
// exit causes.
type childWakePolicy struct {
	runner         *sessionRunner
	h              *subagent.Handle
	childInboxSig  <-chan struct{}
	directInboxSig <-chan struct{}
	steerInboxSig  <-chan struct{}
}

func (p *childWakePolicy) turnCtx(parent context.Context) (context.Context, context.CancelFunc) {
	turnCtx, cancel := context.WithCancel(parent)
	p.h.SetInterruptCancel(cancel)
	return turnCtx, func() {
		p.h.SetInterruptCancel(nil)
		cancel()
	}
}

// drainMidTurn drains the steer inbox at the same safe point the root
// engine uses for steer-message injection. This gives child sessions the
// same steer/follow-up parity the user has at the root: messages sent
// with [subagent.MessageModeSteer] arrive on the steer inbox and land
// during a running turn, while [subagent.MessageModeFollowUp] messages
// sit on the regular inbox and are only consumed between turns.
func (p *childWakePolicy) drainMidTurn(sess *session.Session, events chan Event) bool {
	msgs := p.h.DrainSteerInbox()
	if len(msgs) == 0 {
		return false
	}
	for _, msg := range msgs {
		injectUserMessage(sess, msg.Content, msg.MultiContent, func(ev Event) {
			events <- ev
		})
	}
	return true
}

func (p *childWakePolicy) wakeNext(ctx context.Context, e *sessionEngine, info turnInfo, events chan Event) bool {
	sess := info.sess

	if info.canceled {
		p.h.MarkWaitingSilently()
	} else {
		p.h.PublishTurn(sess.GetLastAssistantMessageContent())
	}

	// Re-park until actual work is injected. Coalesced single-slot signals
	// can fire spuriously when a drain races a publish (e.g. a grandchild
	// envelope that was already coalesced into a pending tick by
	// appendParentEnvelope), or when requestClose calls inbox.Close() and
	// wakes directInboxSig before CloseCh is selected. Running another turn
	// with nothing injected leaves the session ending on an assistant message,
	// which providers like Anthropic reject as unsupported assistant prefill.
	for {
		var injected bool

		select {
		case <-ctx.Done():
			return false
		case <-p.h.CloseCh():
			return false
		case <-p.childInboxSig:
			// If shutdown raced the work signal, close/cancel wins.
			select {
			case <-ctx.Done():
				return false
			case <-p.h.CloseCh():
				return false
			default:
			}

			envs := p.runner.core.subagents.DrainParentInbox(sess.ID)
			if len(envs) == 0 {
				// Stale tick — re-park.
				continue
			}
			for _, env := range envs {
				p.runner.injectSubagentEnvelope(sess, env, events)
			}
			injected = true
		case <-p.directInboxSig:
			// If shutdown raced the work signal, close/cancel wins.
			select {
			case <-ctx.Done():
				return false
			case <-p.h.CloseCh():
				return false
			default:
			}

			msgs := p.h.DrainInbox()
			if len(msgs) == 0 {
				// Either a stale tick or the inbox was closed by
				// requestClose (which also fires CloseCh). Re-park so
				// the CloseCh case takes precedence on the next iteration.
				continue
			}
			for _, msg := range msgs {
				injectUserMessage(sess, msg.Content, msg.MultiContent, func(ev Event) {
					events <- ev
				})
			}
			injected = true

		case <-p.steerInboxSig:
			// Steer-mode messages arriving while the child is parked
			// between turns are delivered the same way as followup
			// messages — plain user content.
			select {
			case <-ctx.Done():
				return false
			case <-p.h.CloseCh():
				return false
			default:
			}

			msgs := p.h.DrainSteerInbox()
			if len(msgs) == 0 {
				continue
			}
			for _, msg := range msgs {
				injectUserMessage(sess, msg.Content, msg.MultiContent, func(ev Event) {
					events <- ev
				})
			}
			injected = true
		}

		if !injected {
			continue
		}

		// One final close/cancel check after draining succeeded but before the
		// handle is transitioned back to Running.
		select {
		case <-ctx.Done():
			return false
		case <-p.h.CloseCh():
			return false
		default:
		}

		p.h.MarkRunning()
		return true
	}
}
