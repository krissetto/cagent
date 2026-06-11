package runtime

import (
	"context"
	"errors"
	"log/slog"

	"github.com/docker/docker-agent/pkg/session"
)

// ensureSessionPersisted creates the session row in the configured store if it
// does not already exist. The SessionRecorder writes transcript mutations and
// assumes the row exists. InMemorySessionStore shares the session pointer with
// the runtime, so AddSession is skipped to avoid double-writing messages into
// the same slice.
func (r *LocalRuntime) ensureSessionPersisted(ctx context.Context, sess *session.Session) {
	if r.sessionStore == nil || sess == nil || sess.ID == "" || sess.ParentID != "" {
		return
	}
	if _, ok := r.sessionStore.(*session.InMemorySessionStore); ok {
		return
	}
	if err := r.sessionStore.AddSession(ctx, sess); err != nil && !errors.Is(err, session.ErrNewerDatabase) {
		slog.WarnContext(ctx, "Failed to persist session start", "session_id", sess.ID, "error", err)
	}
}
