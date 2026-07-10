package runtime

import (
	"context"

	"github.com/docker/docker-agent/pkg/session"
)

// The session actor is backed by a per-session driver. RunOrAttach keeps the
// public contract used by embedders: a free session is driven immediately; a
// busy session is mirrored until it settles, then the caller drives its staged
// turn through the same driver.

func (r *LocalRuntime) StopSession(sessionID string) bool {
	return r.sessionDrivers.StopWake(sessionID)
}

func (r *LocalRuntime) RunOrAttach(ctx context.Context, sess *session.Session) <-chan Event {
	return r.sessionDrivers.Get(sess).RunOrAttach(ctx, sess)
}

func (r *LocalRuntime) sessionSettled(sessionID string) bool {
	return r.sessionDrivers.Settled(sessionID)
}

type SessionRunner interface {
	RunOrAttach(ctx context.Context, sess *session.Session) <-chan Event
}

func RunOrAttachStream(ctx context.Context, rt Runtime, sess *session.Session) <-chan Event {
	if sr, ok := rt.(SessionRunner); ok {
		return sr.RunOrAttach(ctx, sess)
	}
	return rt.RunStream(ctx, sess)
}
