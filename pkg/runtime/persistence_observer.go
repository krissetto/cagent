package runtime

import (
	"context"
	"log/slog"

	"github.com/docker/docker-agent/pkg/session"
)

// PersistenceObserver was the stock [EventObserver] that mirrored the
// runtime's event stream to a [session.Store].
//
// Deprecated: persistence is now handled exclusively by [SessionRecorder],
// which is registered as a global [EventBus] observer during
// [NewLocalRuntime]. PersistenceObserver is retained as a source-compatible
// no-op so that external callers that reference it continue to compile.
// Registering it via [WithEventObserver] is harmless but unnecessary —
// it does no store I/O and will not duplicate writes from the recorder.
type PersistenceObserver struct{}

// OnRunStart is a no-op. Session creation is handled by the runtime's
// direct store calls.
//
// Deprecated: see [PersistenceObserver].
func (p *PersistenceObserver) OnRunStart(_ context.Context, sess *session.Session) {
	// Intentional no-op. The SessionRecorder handles all persistence.
	slog.Debug("PersistenceObserver.OnRunStart called (deprecated no-op)", "session_id", sess.ID)
}

// OnEvent is a no-op. All persistence is handled by [SessionRecorder] via
// the [EventBus] global observer mechanism.
//
// Deprecated: see [PersistenceObserver].
func (p *PersistenceObserver) OnEvent(_ context.Context, _ *session.Session, _ Event) {
	// Intentional no-op. The SessionRecorder handles all persistence.
}
