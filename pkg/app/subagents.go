package app

import (
	"context"
	"log/slog"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

// subagentRuntime is the optional capability surface the App needs from a
// runtime for the async subagent feature. The local runtime implements it;
// runtimes that don't (e.g. remote) degrade every use below to a no-op. The
// methods are one cohesive feature, so they are asserted as one interface.
type subagentRuntime interface {
	// DeliverMessage delivers a detached user message to a session by id.
	DeliverMessage(ctx context.Context, sessionID, content string) bool
	// SubagentTree is the live swarm registry, for snapshot subscriptions.
	SubagentTree() *subagent.Tree
	// SubscribeSessionEvents mirrors a session's run events to a viewer.
	SubscribeSessionEvents(sessionID string) (seed []runtime.Event, events <-chan runtime.Event, cancel func())
	// RestoreSubagentTree rebuilds a reloaded session's swarm from the store.
	RestoreSubagentTree(ctx context.Context, sess *session.Session) (*subagent.Snapshot, error)
	// StopSession cancels a live runtime-owned wake run for the session.
	StopSession(sessionID string) bool
}

// WithSubagentAttach marks the App as a live viewer of an async subagent's
// sub-session. The App mirrors the session's run events onto its bus, and
// hands user input to the runtime for delivery so the subagent processes it
// like any other input.
func WithSubagentAttach(info runtime.SubagentAttachInfo) Opt {
	return func(a *App) {
		a.attachedSubagent = &info
	}
}

// AttachedSubagent returns the attach info when this App is a live viewer of
// an async subagent's sub-session, or nil for a regular App.
func (a *App) AttachedSubagent() *runtime.SubagentAttachInfo {
	return a.attachedSubagent
}

// reloadSubagentTree rebuilds the loaded session's subagent swarm from the
// subagent store (session stores don't carry the snapshot — it lives in its
// own table/backend) and populates the session's view of it before the TUI
// reads it. Restored subagents stay conversational: the runtime adopts them
// as idle actors. No-op when the session already holds a (live) tree.
func (a *App) reloadSubagentTree(ctx context.Context) {
	if a.session == nil || a.session.GetSubagentTree() != nil {
		return
	}
	rt, ok := a.runtime.(subagentRuntime)
	if !ok {
		return
	}
	snap, err := rt.RestoreSubagentTree(ctx, a.session)
	if err != nil {
		slog.WarnContext(ctx, "Failed to restore subagent tree for session", "session_id", a.session.ID, "error", err)
		return
	}
	if snap != nil {
		a.session.SetSubagentTree(snap)
	}
}

// startSessionEventBridge mirrors the App's session's run events onto the
// App bus — the single event source for local runtimes. Whoever drives a run
// (this App's own turns, the subagent manager for attached children, or the
// runtime's session actor waking the session with a subagent report), the
// bus carries the same ordered stream. Seed events (the head of an in-flight
// run when subscribing mid-stream) are forwarded first, then the live
// channel. Restartable: a new call replaces the previous session's bridge
// (session switches). Reports whether a bridge is active, so Run knows the
// bus is fed and its own channel is flow-control only.
func (a *App) startSessionEventBridge(ctx context.Context) bool {
	if a.stopBridge != nil {
		a.stopBridge()
		a.stopBridge = nil
	}
	rt, ok := a.runtime.(subagentRuntime)
	if !ok || a.session == nil {
		return false
	}
	bridgeCtx, cancel := context.WithCancel(ctx)
	a.stopBridge = cancel
	seed, ch, cancelSub := rt.SubscribeSessionEvents(a.session.ID)
	// The hub drops events for subscribers whose buffer is full — fine for
	// casual viewers, not for the tab's only event source. The elastic pump
	// drains the subscription promptly regardless of bus pressure, so the
	// stream the user watches is lossless, like the per-run channels were.
	forwardToBus(bridgeCtx, a, seed, pumpElastic(bridgeCtx, ch), cancelSub, a.filterBridgedEvent)
	return true
}

// pumpElastic relays in to the returned channel through an unbounded
// in-memory queue: reads from in never wait on the consumer. Closes the
// output when in closes and the queue is drained, or when ctx ends.
func pumpElastic[T any](ctx context.Context, in <-chan T) <-chan T {
	out := make(chan T)
	go func() {
		defer close(out)
		var queue []T
		for {
			var send chan<- T
			var head T
			if len(queue) > 0 {
				send = out
				head = queue[0]
			} else if in == nil {
				return
			}
			select {
			case <-ctx.Done():
				return
			case v, ok := <-in:
				if !ok {
					in = nil
					continue
				}
				queue = append(queue, v)
			case send <- head:
				queue = queue[1:]
			}
		}
	}()
	return out
}

// filterBridgedEvent applies the App's own-run semantics to the bridged
// stream: while the user has cancelled the in-flight turn, everything but
// the stream-stop is dropped — and that stop releases the gate, so it mutes
// exactly the cancelled run's tail and later runs render normally. A retry's
// pre-StreamStarted user-message re-emission is suppressed (the bubble is
// already on screen), and a session title arriving from any run clears the
// title-generating flag.
func (a *App) filterBridgedEvent(e runtime.Event) runtime.Event {
	switch e.(type) {
	case *runtime.SessionTitleEvent:
		a.titleGenerating.Store(false)
	case *runtime.StreamStartedEvent:
		a.suppressUserEcho.Store(false)
	case *runtime.UserMessageEvent:
		if a.suppressUserEcho.Load() {
			return nil
		}
	case *runtime.StreamStoppedEvent:
		a.runCancelled.Store(false)
		return e
	}
	if a.runCancelled.Load() {
		return nil
	}
	return e
}

// startSubagentTreeBridge mirrors live swarm snapshots onto the App bus so the
// sidebar can render a running-subagent tree.
func (a *App) startSubagentTreeBridge(ctx context.Context) {
	rt, ok := a.runtime.(subagentRuntime)
	if !ok {
		return
	}
	ch, cancel := rt.SubagentTree().Subscribe(16)
	forwardToBus(ctx, a, nil, ch, cancel, runtime.SubagentTree)
}

// forwardToBus pumps seed then ch onto the App's event bus, wrapping each
// value, until ctx is done or ch closes. A nil wrap result drops the value.
// cancel releases the subscription.
func forwardToBus[T any](ctx context.Context, a *App, seed []T, ch <-chan T, cancel func(), wrap func(T) runtime.Event) {
	go func() {
		defer cancel()
		send := func(v T) bool {
			e := wrap(v)
			if e == nil {
				return true
			}
			select {
			case a.events <- e:
				return true
			case <-ctx.Done():
				return false
			}
		}
		for _, v := range seed {
			if !send(v) {
				return
			}
		}
		for {
			select {
			case <-ctx.Done():
				return
			case v, ok := <-ch:
				if !ok || !send(v) {
					return
				}
			}
		}
	}()
}

// emitLiveSubagentTree emits the runtime's current subagent snapshot when it
// covers the active session. An empty or foreign snapshot is not emitted — it
// would wipe a restored view that the live tree knows nothing about.
func (a *App) emitLiveSubagentTree(ctx context.Context) {
	rt, ok := a.runtime.(subagentRuntime)
	if !ok || a.session == nil {
		return
	}
	snap := rt.SubagentTree().Snapshot()
	rootID := subagent.SessionRootID(a.session.ID)
	for _, n := range snap.Nodes {
		if n.Node.ID == rootID {
			select {
			case a.events <- runtime.SubagentTree(snap):
			case <-ctx.Done():
			}
			return
		}
	}
}
