package runtime

import (
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

// appendSubagentEnvelopeToSession records the envelope as an implicit user
// message in sess and emits the corresponding observability events through
// the supplied emit callback.
//
// All subagent-envelope injection flows through this single helper, whether
// the injection happens mid-turn (at the post-tool-calls safe point) or
// between turns (while the parent is idle).

// drainParentEnvelopesMidTurn consumes child→parent envelopes at the
// post-tool-call safe point for root sessions only. Child sessions receive
// their own descendant envelopes through their wake policy; draining here
// would let them observe parent-level inboxes at the wrong point.
func (r *LocalRuntime) drainParentEnvelopesMidTurn(sess *session.Session, state *sessionState, events EventSink) bool {
	if r.subagents == nil || state.isChild {
		return false
	}
	envs := r.subagents.DrainParentInbox(sess.ID)
	if len(envs) == 0 {
		return false
	}
	for _, env := range envs {
		r.appendSubagentEnvelopeToSession(sess, env, func(ev Event) { events.Emit(ev) })
	}
	return true
}

func (r *LocalRuntime) appendSubagentEnvelopeToSession(sess *session.Session, env subagent.Envelope, emit func(Event)) {
	content := subagent.FormatEnvelopeMessage(env)

	user := session.SubagentEnvelopeMessage(content)
	pos := sess.AppendMessage(user)

	ev := TypedUserMessage(session.MessageKindSubagentEnvelope, content, sess.ID, nil, pos)
	emit(ev)
	emit(SubAgentUpdate(env, sess.ID))
}

// publishTreeChangeFromChild publishes a LiveSessionTreeChangedEvent to the
// event bus topic of every ancestor session above immediateParentID. The
// immediate parent is excluded because it already receives the specific
// SubAgentStarted / SubAgentUpdate event through its own events channel.
//
// This helper is wired into both the Manager's onEnvelopePublished and
// onChildRegistered hooks so ancestor sessions learn about nested subagent
// creation and state changes.
func (r *LocalRuntime) publishTreeChangeFromChild(childID, immediateParentID string) {
	if r.subagents == nil {
		return
	}
	for _, ancID := range r.subagents.Ancestors(childID) {
		if ancID == immediateParentID {
			continue
		}
		r.publishSessionEvent(ancID, LiveSessionTreeChanged(ancID, ""))
	}
}

// publishSessionEvent publishes an event on the EventBus for a specific
// session. This is the runtime's internal broadcaster for non-stream events
// (tree change notifications, etc.).
func (r *LocalRuntime) publishSessionEvent(sessionID string, ev Event) {
	if r.eventBus != nil {
		r.eventBus.Publish(sessionID, ev)
	}
}
