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
	// RegisterMessageReceiver wires delivery of messages (notes from async
	// subagents) for an idle session; the returned func unregisters it.
	RegisterMessageReceiver(sessionID string, fn runtime.MessageReceiver) func()
	// DeliverMessage delivers a detached user message to a session by id.
	DeliverMessage(ctx context.Context, sessionID, content string) bool
	// SubagentTree is the live swarm registry, for snapshot subscriptions.
	SubagentTree() *subagent.Tree
	// SubscribeSessionEvents mirrors a session's run events to a viewer.
	SubscribeSessionEvents(sessionID string) (seed []runtime.Event, events <-chan runtime.Event, cancel func())
	// RestoreSubagentTree rebuilds a reloaded session's swarm from the store.
	RestoreSubagentTree(ctx context.Context, sess *session.Session) (*subagent.Snapshot, error)
}

// WithSubagentAttach marks the App as a live viewer of an async subagent's
// sub-session. The App does not register a message receiver (the runtime's
// subagent manager owns the session), mirrors the session's run events onto
// its bus, and hands user input to the runtime for delivery so the subagent
// processes it like any other input.
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

// registerMessageReceiver wires the current session's detached-message
// delivery (notes from async subagents). The runtime steers messages into a
// live run loop itself and only calls the receiver when the session is idle,
// so routing through the normal SendMsg path starts a fresh run immediately.
func (a *App) registerMessageReceiver() {
	if a.unregisterReceiver != nil {
		a.unregisterReceiver()
		a.unregisterReceiver = nil
	}
	if a.session == nil {
		return
	}
	rt, ok := a.runtime.(subagentRuntime)
	if !ok {
		return
	}
	a.unregisterReceiver = rt.RegisterMessageReceiver(a.session.ID, func(ctx context.Context, content string) {
		a.InjectUserMessage(ctx, content)
	})
}

// startSessionEventBridge mirrors the attached session's run events onto the
// App bus: the subagent manager drives the session (spawn task, parent
// messages, re-runs); this App just watches everything those runs
// emit — streaming deltas, tool calls, steered user messages, lifecycle.
// Seed events (the head of an in-flight assistant message when attaching
// mid-stream) are forwarded first, then the live channel.
func (a *App) startSessionEventBridge(ctx context.Context) {
	rt, ok := a.runtime.(subagentRuntime)
	if !ok {
		return
	}
	seed, ch, cancel := rt.SubscribeSessionEvents(a.session.ID)
	forwardToBus(ctx, a, seed, ch, cancel, func(e runtime.Event) runtime.Event { return e })
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
// value, until ctx is done or ch closes. cancel releases the subscription.
func forwardToBus[T any](ctx context.Context, a *App, seed []T, ch <-chan T, cancel func(), wrap func(T) runtime.Event) {
	go func() {
		defer cancel()
		send := func(v T) bool {
			select {
			case a.events <- wrap(v):
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
