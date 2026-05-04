// subagent_envelopes.go contains envelope rendering plus ancestor tree
// notification helpers. Session-local inbox draining and idle-wait logic
// now lives on [sessionRunner], which owns the per-session coordination
// state used by both root and child engines.
package runtime

import (
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

// appendSubagentEnvelopeToSession records the envelope as an implicit user
// message in sess and emits the corresponding observability events through the
// supplied callback.
//
// All subagent-envelope injection now flows through the live engine events
// channel via [sessionRunner.injectSubagentEnvelope]. The channel is owned for
// the whole session lifetime by [sessionRunner.runStreamWithConfig] and is
// fanned out to observers by [LocalRuntime.wrapEventsForObservers], so both
// active-turn and idle-between-turn injections use the same code path.
func (r *LocalRuntime) appendSubagentEnvelopeToSession(sess *session.Session, env subagent.Envelope, emit func(Event)) {
	content := subagent.FormatEnvelopeMessage(env)
	user := session.UserMessage(content)
	// Mark implicit so UI clients can style these differently from real user
	// input.
	user.Implicit = true
	user.Kind = session.MessageKindSubagentEnvelope
	pos := sess.AddMessage(user)
	emit(TypedUserMessage(session.MessageKindSubagentEnvelope, content, sess.ID, nil, pos))
	emit(SubAgentUpdate(env, sess.ID))
	// Ancestor notification is handled unconditionally by the Manager's
	// onEnvelopePublished hook at publish time (not drain time), so we do
	// NOT call publishTreeChangeFromChild here.
}

// publishTreeChangeFromChild publishes a [LiveSessionTreeChangedEvent] to the
// event bus of every ancestor session of the given child. This is the single
// helper wired into both the Manager's onEnvelopePublished and
// onChildRegistered hooks so ancestor tabs learn about nested subagent
// creation and state changes without waiting for intermediate envelopes to
// propagate up the chain.
//
// The immediate parent is excluded because it already receives the specific
// SubAgentStarted / SubAgentUpdate event through the normal events channel.
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
