package runtime

import (
	"context"
	"errors"
)

// LiveEventSource opens a live event stream for a session by id.
//
// It is the minimal surface the TUI layer needs to attach to an arbitrary
// live session (root or descendant) without knowing whether the session
// is owned by an in-process runtime or served over HTTP. Both the
// in-process LocalRuntime and the HTTP *Client satisfy this interface.
//
// Callers are expected to cancel ctx when they no longer want events;
// implementations must close the returned channel when ctx is cancelled
// or the underlying session ends.
type LiveEventSource interface {
	AttachLiveSession(ctx context.Context, sessionID string) (<-chan Event, error)
}

// LiveEventSourceWithSnapshot is an optional extension of [LiveEventSource]
// that lets attach callers receive the topic's current in-progress assistant
// content alongside the live event stream. The snapshot is captured atomically
// with the subscription so there is no gap between the replayed content and the
// first event read from the stream.
type LiveEventSourceWithSnapshot interface {
	AttachLiveSessionWithSnapshot(ctx context.Context, sessionID string) (<-chan Event, StreamingSnapshot, error)
}

// Compile-time assertion that the HTTP client satisfies LiveEventSource.
// LocalRuntime satisfies it via the method defined below.
var (
	_ LiveEventSource             = (*Client)(nil)
	_ LiveEventSource             = (*LocalRuntime)(nil)
	_ LiveEventSourceWithSnapshot = (*LocalRuntime)(nil)
)

// AttachLiveSession opens a live event stream for the given session id by
// subscribing to this runtime's per-session event bus. It is the in-process
// counterpart to Client.AttachLiveSession and satisfies LiveEventSource so
// the TUI can treat local and remote attachments uniformly.
//
// The session must be live within this runtime's tree: either a root session
// owned by this runtime or a descendant subagent registered with it. Events
// stream until ctx is cancelled or the underlying session ends.
func (r *LocalRuntime) AttachLiveSession(ctx context.Context, sessionID string) (<-chan Event, error) {
	if sessionID == "" {
		return nil, errors.New("session id is required")
	}
	sub := r.SubscribeSession(ctx, sessionID, 128)
	return sub.Events, nil
}

// AttachLiveSessionWithSnapshot satisfies [LiveEventSourceWithSnapshot]. It
// behaves identically to [AttachLiveSession] but also returns the topic's
// current streaming snapshot, captured atomically with the subscription so the
// caller can replay any assistant content that streamed before this attach.
func (r *LocalRuntime) AttachLiveSessionWithSnapshot(ctx context.Context, sessionID string) (<-chan Event, StreamingSnapshot, error) {
	if sessionID == "" {
		return nil, StreamingSnapshot{}, errors.New("session id is required")
	}
	sub, snapshot := r.eventBus.SubscribeWithSnapshot(ctx, sessionID, 128)
	return sub.Events, snapshot, nil
}
