// live_session_runtime.go groups the runtime capabilities needed to observe
// and control a live session tree on a single value while preserving the
// smaller public capability interfaces for narrow callers.

package runtime

import "context"

// SessionObserverSubscriber is implemented by runtimes that can subscribe
// an arbitrary caller to a specific session's event stream.
type SessionObserverSubscriber interface {
	// SubscribeSession registers a new observer on the given session id and
	// returns a Subscription whose Events channel delivers live events until
	// Cancel is called or ctx is cancelled.
	SubscribeSession(ctx context.Context, sessionID string, buffer int) *Subscription
}

// SessionTreeProvider is implemented by runtimes that can return the live
// session tree rooted at a given session id.
type SessionTreeProvider interface {
	// LiveSessionTree returns the tree of live sessions rooted at rootID.
	LiveSessionTree(rootSessionID string) (*SessionTree, error)
}

// LiveSessionRuntime aggregates the runtime capabilities needed to observe
// and control a live session tree (root + descendant subagents). It is the
// preferred surface for new TUI/server code that wants the full
// observe-and-control vocabulary on a single value.
//
// Existing capability interfaces ([LiveEventSource], [LiveEventSourceWithSnapshot])
// remain valid public surface and are intentionally additive: external
// consumers may still depend on them, and small interfaces stay idiomatic
// for narrow callers.
type LiveSessionRuntime interface {
	LiveEventSource
	LiveEventSourceWithSnapshot
	SessionTreeProvider

	// SteerSessionByID sends a mid-turn steer message to the given session.
	SteerSessionByID(sessionID string, msg QueuedMessage) error

	// FollowUpSessionByID enqueues a follow-up message for the given session.
	FollowUpSessionByID(sessionID string, msg QueuedMessage) error

	// InterruptSessionByID cancels the current turn of the given subagent session.
	InterruptSessionByID(sessionID string) error

	// CloseSessionByID asks the given subagent session to close cleanly.
	CloseSessionByID(sessionID string) error

	// StopSessionByID forcibly stops the given subagent session.
	StopSessionByID(sessionID string) error
}

// Compile-time assertion: LocalRuntime satisfies LiveSessionRuntime.
var _ LiveSessionRuntime = (*LocalRuntime)(nil)
