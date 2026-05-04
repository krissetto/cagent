// live_session_runtime.go groups the runtime capabilities needed to observe
// and control a live session tree on a single value while preserving the
// smaller public capability interfaces for narrow callers.
package runtime

// LiveSessionRuntime aggregates the runtime capabilities needed to observe
// and control a live session tree (root + descendant subagents). It is the
// preferred surface for new TUI/server code that wants the full
// observe-and-control vocabulary on a single value.
//
// Existing capability interfaces ([SessionObserverSubscriber],
// [SessionTreeProvider], [SessionStartupEmitter], [LiveSessionProvider],
// [LiveEventSource], [LiveEventSourceWithSnapshot]) remain valid public
// surface and are intentionally NOT removed: external consumers may still
// depend on them, and small interfaces stay idiomatic for narrow callers.
type LiveSessionRuntime interface {
	SessionObserverSubscriber
	SessionTreeProvider
	SessionStartupEmitter
	LiveSessionProvider
	LiveEventSource
	LiveEventSourceWithSnapshot
}

var _ LiveSessionRuntime = (*LocalRuntime)(nil)
