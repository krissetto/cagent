// File wake_policy.go contains the per-mode wake policies that decide what to do
// after the run loop completes a turn: root sessions consume follow-ups and
// keep alive while subagents are in flight; child sessions publish a turn
// envelope and wait on parent/descendant inbox traffic.

package runtime

import (
	"context"
	"log/slog"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

// wakePolicy is the only point of variation between the root in-process loop
// and child subagent loops.
//
// Implementations decide what should happen *after* the model emits a stopped
// assistant turn. For root sessions this is "drain the follow-up queue,
// otherwise wait for subagent inbox traffic, otherwise terminate". For child
// sessions it is "publish the turn envelope, wait on the parent inbox /
// descendant signal / close request".
type wakePolicy interface {
	// turnCtx wraps the loop's context for the upcoming turn. Root sessions
	// return ctx unchanged; child sessions derive a per-turn cancel so users
	// can interrupt a single turn without killing the subagent.
	turnCtx(parent context.Context) (context.Context, context.CancelFunc)

	// wakeNext is called once the model signals stop and the turn has ended.
	// It may block until new work arrives (follow-up, parent message,
	// descendant envelope, ...) and inject whatever session mutations are
	// needed before returning. Returning true means the loop should run
	// another turn; false means the loop should exit cleanly.
	wakeNext(ctx context.Context, r *LocalRuntime, state *sessionState, sess *session.Session, responseContent string, messageCountBeforeTools int, contextLimit int64, events EventSink) bool

	// drainMidTurn is called at the post-tool-calls safe point. Returns
	// true when at least one message was injected. Root returns false
	// (dedicated steer/envelope drains); child drains its steer inbox.
	drainMidTurn(sess *session.Session, events EventSink) bool
}

// rootWakePolicy is the policy used by RunStream for the root session.
type rootWakePolicy struct{}

func (rootWakePolicy) turnCtx(parent context.Context) (context.Context, context.CancelFunc) {
	return parent, func() {} // root sessions don't need per-turn cancellation
}

func (rootWakePolicy) drainMidTurn(_ *session.Session, _ EventSink) bool {
	return false // root drains are handled inline in the loop
}

func (rootWakePolicy) wakeNext(ctx context.Context, r *LocalRuntime, state *sessionState, sess *session.Session, responseContent string, messageCountBeforeTools int, contextLimit int64, events EventSink) bool {
	a := r.resolveSessionAgent(sess)

	slog.DebugContext(ctx, "Conversation stopped", "agent", a.Name())
	r.executeStopHooks(ctx, sess, a, responseContent, events)

	// --- FOLLOW-UP: end-of-turn injection ---
	if followUp, ok := state.followUpQueue.Dequeue(ctx); ok {
		injectUserMessage(sess, followUp.Content, followUp.MultiContent, func(ev Event) { events.Emit(ev) })
		modelID := r.getEffectiveModelID(a)
		m, _ := r.modelsStore.GetModel(ctx, modelID)
		r.compactIfNeeded(ctx, sess, a, m, contextLimit, messageCountBeforeTools, events)
		return true
	}

	// Re-check steer queue: closes the race between the mid-loop drain and stop.
	if drained, _ := r.drainAndEmitSteered(ctx, state, sess, events); drained {
		modelID := r.getEffectiveModelID(a)
		m, _ := r.modelsStore.GetModel(ctx, modelID)
		r.compactIfNeeded(ctx, sess, a, m, contextLimit, messageCountBeforeTools, events)
		return true
	}

	// Block on subagent inbox traffic when descendants are in flight.
	if r.waitForSubagentInbox(ctx, state, sess, events) {
		modelID := r.getEffectiveModelID(a)
		m, _ := r.modelsStore.GetModel(ctx, modelID)
		r.compactIfNeeded(ctx, sess, a, m, contextLimit, messageCountBeforeTools, events)
		return true
	}

	return false
}

// childWakePolicy is the policy used by subagent child sessions.
type childWakePolicy struct {
	r             *LocalRuntime
	h             *subagent.Handle
	childInboxSig <-chan struct{}
}

func (p *childWakePolicy) turnCtx(parent context.Context) (context.Context, context.CancelFunc) {
	turnCtx, cancel := context.WithCancel(parent)
	p.h.SetInterruptCancel(cancel)
	return turnCtx, func() {
		p.h.SetInterruptCancel(nil)
		cancel()
	}
}

// drainMidTurn drains the steer inbox at the post-tool-calls safe point.
func (p *childWakePolicy) drainMidTurn(sess *session.Session, events EventSink) bool {
	msgs := p.h.DrainSteerInbox()
	if len(msgs) == 0 {
		return false
	}
	for _, msg := range msgs {
		injectUserMessage(sess, msg.Content, msg.MultiContent, func(ev Event) {
			events.Emit(ev)
		})
	}
	return true
}

func (p *childWakePolicy) wakeNext(ctx context.Context, r *LocalRuntime, state *sessionState, sess *session.Session, _ string, _ int, _ int64, events EventSink) bool {
	// On a per-turn interrupt the child should park silently rather
	// than publish a fresh turn-completed envelope (matches HEAD UX:
	// interrupt cancels the in-flight turn but keeps the subagent
	// available for follow-up). Status stays Waiting so the parent's
	// HasInFlightChildren check correctly drops to false until new
	// work arrives.
	if state != nil && state.interruptedTurn {
		state.interruptedTurn = false
		p.h.MarkWaitingSilently()
	} else {
		p.h.PublishTurn(sess.GetLastAssistantMessageContent())
	}

	// Re-park until actual work is injected.
	for {
		var injected bool

		select {
		case <-ctx.Done():
			return false
		case <-p.h.CloseCh():
			return false
		case <-p.childInboxSig:
			select {
			case <-ctx.Done():
				return false
			case <-p.h.CloseCh():
				return false
			default:
			}

			envs := r.subagents.DrainParentInbox(sess.ID)
			if len(envs) == 0 {
				continue
			}
			for _, env := range envs {
				r.appendSubagentEnvelopeToSession(sess, env, func(ev Event) { events.Emit(ev) })
			}
			injected = true
		case <-p.h.InboxSignal():
			select {
			case <-ctx.Done():
				return false
			case <-p.h.CloseCh():
				return false
			default:
			}

			msgs := p.h.DrainInbox()
			if len(msgs) == 0 {
				continue
			}
			for _, msg := range msgs {
				injectUserMessage(sess, msg.Content, msg.MultiContent, func(ev Event) {
					events.Emit(ev)
				})
			}
			injected = true
		case <-p.h.SteerInboxSignal():
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
					events.Emit(ev)
				})
			}
			injected = true
		}

		if !injected {
			continue
		}

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

// waitForSubagentInbox blocks the root loop when live descendants are still
// in flight. On wake it drains the parent's inbox into the session as
// envelope messages, emits ParentIdle/ParentResume events, and returns true
// to indicate the loop should continue.
func (r *LocalRuntime) waitForSubagentInbox(ctx context.Context, state *sessionState, sess *session.Session, events EventSink) bool {
	if r.subagents == nil || !r.subagents.HasInFlightChildren(sess.ID) {
		return false
	}

	a := r.resolveSessionAgent(sess)
	events.Emit(ParentIdle(sess.ID, a.Name()))

	parentInboxSig := r.subagents.ParentInboxSignal(sess.ID)
	steerSig := steerSignalChan(state.steerQueue)
	followUpSig := steerSignalChan(state.followUpQueue)
	for {
		select {
		case <-ctx.Done():
			return false
		case <-parentInboxSig:
			envs := r.subagents.DrainParentInbox(sess.ID)
			if len(envs) == 0 {
				// Stale tick. Check follow-up/steer fallthrough.
				if drained, _ := r.drainAndEmitSteered(ctx, state, sess, events); drained {
					events.Emit(ParentResume(sess.ID, a.Name()))
					return true
				}
				if followUp, ok := state.followUpQueue.Dequeue(ctx); ok {
					injectUserMessage(sess, followUp.Content, followUp.MultiContent, func(ev Event) { events.Emit(ev) })
					events.Emit(ParentResume(sess.ID, a.Name()))
					return true
				}
				if !r.subagents.HasInFlightChildren(sess.ID) {
					return false
				}
				continue
			}
			for _, env := range envs {
				r.appendSubagentEnvelopeToSession(sess, env, func(ev Event) { events.Emit(ev) })
			}
			events.Emit(ParentResume(sess.ID, a.Name()))
			return true
		case <-steerSig:
			if drained, _ := r.drainAndEmitSteered(ctx, state, sess, events); drained {
				events.Emit(ParentResume(sess.ID, a.Name()))
				return true
			}
		case <-followUpSig:
			if followUp, ok := state.followUpQueue.Dequeue(ctx); ok {
				injectUserMessage(sess, followUp.Content, followUp.MultiContent, func(ev Event) { events.Emit(ev) })
				events.Emit(ParentResume(sess.ID, a.Name()))
				return true
			}
		}
	}
}

// steerSignalChan returns a signal-only notification channel for the given
// MessageQueue without consuming actual messages. It type-asserts the
// optional interface{ Signal() <-chan struct{} } that inMemoryMessageQueue
// implements. Foreign MessageQueue implementations that don't implement
// Signal fall back to nil (the select case never fires); they will still
// wake on the parent inbox signal or context cancellation.
func steerSignalChan(q MessageQueue) <-chan struct{} {
	if sq, ok := q.(interface{ Signal() <-chan struct{} }); ok {
		return sq.Signal()
	}
	return nil
}
