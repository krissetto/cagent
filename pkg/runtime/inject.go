package runtime

import (
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
)

// injectUserMessage appends a user-role message to sess and emits a
// corresponding [UserMessageEvent] via emit.
//
// All inbound paths (root steer, root follow-up, parent→child steer,
// parent→child follow-up, descendant envelope) converge on this single
// helper. Content is appended verbatim — no wrapper is added.
// Delivery timing is the responsibility of the calling drain, not the
// content-injection helper.
//
// emit is provided by the caller so the same helper works for both
// callers that own a live RunStream events channel
// (`func(ev Event) { events <- ev }`) and callers that publish directly
// to the session's event-bus topic
// (`func(ev Event) { r.publishSessionEvent(sess.ID, ev) }`).
func injectUserMessage(sess *session.Session, content string, multiContent []chat.MessagePart, emit func(Event)) {
	user := session.UserMessage(content, multiContent...)
	idx := sess.AddMessage(user)
	emit(UserMessage(content, sess.ID, multiContent, idx))
}
