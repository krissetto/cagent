package runtime

import (
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
)

// injectUserMessage appends a user-role message to sess and emits the
// corresponding UserMessageEvent via emit. It returns the appended message
// index in sess.Messages.
//
// All inbound paths (root steer, root follow-up, parent→child steer,
// parent→child follow-up, descendant envelope) should converge on this helper.
func injectUserMessage(sess *session.Session, content string, multiContent []chat.MessagePart, emit func(Event)) {
	user := session.UserMessage(content, multiContent...)

	idx := sess.AppendMessage(user)

	emit(UserMessage(content, sess.ID, multiContent, idx))
}
