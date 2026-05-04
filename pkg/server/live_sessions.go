package server

import (
	"context"
	"errors"
	"fmt"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

// resolveLiveSession looks up the runtime that owns the given session id,
// whether sessionID is a root session managed by [SessionManager] directly or
// a subagent descendant living inside one of those root runtimes.
//
// The returned [*activeRuntimes] is non-nil ONLY when sessionID is a root
// session that the manager indexes directly. For descendants the manager has
// no per-descendant activeRuntimes entry, so the second return value is nil
// even though the resolver successfully located the owning runtime. Callers
// that need root-session-only fields like [activeRuntimes.session] should
// gate on a non-nil art rather than re-deriving the root/descendant
// distinction from tree contents.
//
// This is the single shared resolver used by every live-session operation
// the manager exposes (lookup, attach, snapshot, control). Callers that need
// the richer [liveSessionOwner] capability should type-assert on the returned
// runtime; resolveLiveSession does not require it because most callers only
// need tree + observer features.
//
// Returns an error wrapping the session id when the session is not found.
func (sm *SessionManager) resolveLiveSession(sessionID string) (liveSessionRuntime, *activeRuntimes, error) {
	// Root session fast path: the manager itself indexes root runtimes by
	// their own session id.
	if art, ok := sm.runtimeSessions.Load(sessionID); ok {
		treeRt, supports := art.runtime.(liveSessionRuntime)
		if !supports {
			return nil, nil, errors.New("runtime does not support live session")
		}
		return treeRt, art, nil
	}

	// Descendant search: walk every root runtime and ask whether it knows
	// about a live session with this id. Descendants intentionally return a
	// nil activeRuntimes so callers cannot accidentally treat the owning
	// root's session pointer as the descendant's own session pointer.
	var found liveSessionRuntime
	sm.runtimeSessions.Range(func(_ string, art *activeRuntimes) bool {
		treeRt, supports := art.runtime.(liveSessionRuntime)
		if !supports {
			return true
		}
		if _, exists := treeRt.LiveSessionNode(sessionID); !exists {
			return true
		}
		found = treeRt
		return false
	})
	if found == nil {
		return nil, nil, fmt.Errorf("live session %s not found", sessionID)
	}
	return found, nil, nil
}

// LiveSessionTree returns the current live session tree rooted at the
// given root session id. The root node is always included when a
// runtime exists for that root session.
func (sm *SessionManager) LiveSessionTree(rootSessionID string) ([]runtime.LiveSessionNode, error) {
	rt, exists := sm.runtimeSessions.Load(rootSessionID)
	if !exists {
		return nil, errors.New("session not found or not live")
	}
	treeRt, ok := rt.runtime.(liveSessionRuntime)
	if !ok {
		return nil, errors.New("runtime does not support live session trees")
	}
	return treeRt.LiveSessionTree(rootSessionID), nil
}

// LiveSessionNode resolves an arbitrary live session node by id across
// all currently-running root runtimes.
func (sm *SessionManager) LiveSessionNode(sessionID string) (runtime.LiveSessionNode, error) {
	treeRt, _, err := sm.resolveLiveSession(sessionID)
	if err != nil {
		return runtime.LiveSessionNode{}, err
	}
	// For root sessions the resolver hands back the owning runtime; the
	// node lives at index 0 of that runtime's tree. Descendants are
	// resolved through LiveSessionNode directly.
	if node, ok := treeRt.LiveSessionNode(sessionID); ok {
		return node, nil
	}
	nodes := treeRt.LiveSessionTree(sessionID)
	if len(nodes) > 0 {
		return nodes[0], nil
	}
	return runtime.LiveSessionNode{}, errors.New("live session not found")
}

// AttachLiveSession subscribes to the live event stream for the given
// session id, whether root or descendant.
func (sm *SessionManager) AttachLiveSession(ctx context.Context, sessionID string, buffer int) (*runtime.Subscription, error) {
	treeRt, _, err := sm.resolveLiveSession(sessionID)
	if err != nil {
		return nil, err
	}
	obs, ok := treeRt.(runtime.SessionObserverSubscriber)
	if !ok {
		return nil, errors.New("runtime does not support live session observers")
	}
	return obs.SubscribeSession(ctx, sessionID, buffer), nil
}

// SteerLiveSession injects messages into an arbitrary live session.
func (sm *SessionManager) SteerLiveSession(_ context.Context, sessionID string, messages []api.Message) error {
	for _, msg := range messages {
		if err := sm.controlLiveSession(sessionID, func(rt liveSessionRuntime) error {
			return rt.SteerSessionByID(sessionID, runtime.QueuedMessage{Content: msg.Content, MultiContent: msg.MultiContent})
		}); err != nil {
			return err
		}
	}
	return nil
}

// FollowUpLiveSession queues messages for an arbitrary live session.
func (sm *SessionManager) FollowUpLiveSession(_ context.Context, sessionID string, messages []api.Message) error {
	for _, msg := range messages {
		if err := sm.controlLiveSession(sessionID, func(rt liveSessionRuntime) error {
			return rt.FollowUpSessionByID(sessionID, runtime.QueuedMessage{Content: msg.Content, MultiContent: msg.MultiContent})
		}); err != nil {
			return err
		}
	}
	return nil
}

// CloseLiveSession asks a live descendant session to close cleanly.
func (sm *SessionManager) CloseLiveSession(_ context.Context, sessionID string) error {
	return sm.controlLiveSession(sessionID, func(rt liveSessionRuntime) error {
		return rt.CloseSessionByID(sessionID)
	})
}

// InterruptLiveSession cancels the currently-running turn of a live
// descendant session without terminating the session itself.
func (sm *SessionManager) InterruptLiveSession(_ context.Context, sessionID string) error {
	return sm.controlLiveSession(sessionID, func(rt liveSessionRuntime) error {
		return rt.InterruptSessionByID(sessionID)
	})
}

// LiveSessionSnapshot returns the live session pointer plus the current
// live-tree metadata for the given id, whether it is a root session or a
// descendant subagent.
//
// The returned [*session.Session] is the actual in-memory session used by the
// running agent loop, so callers should treat it as read-only and pair it with
// AttachLiveSession for subsequent updates.
func (sm *SessionManager) LiveSessionSnapshot(_ context.Context, sessionID string) (*session.Session, runtime.LiveSessionNode, error) {
	treeRt, art, err := sm.resolveLiveSession(sessionID)
	if err != nil {
		return nil, runtime.LiveSessionNode{}, err
	}

	// Root session: the resolver returns the owning activeRuntimes entry
	// only when sessionID is a root id, so a non-nil art is an unambiguous
	// signal that we should hand back the runtime's owning session pointer.
	if art != nil && art.session != nil {
		nodes := treeRt.LiveSessionTree(sessionID)
		if len(nodes) > 0 {
			return art.session, nodes[0], nil
		}
	}

	// Descendant: hand out the live subagent session pointer when the
	// runtime exposes it.
	owner, ok := treeRt.(liveSessionOwner)
	if !ok {
		return nil, runtime.LiveSessionNode{}, errors.New("runtime does not expose live subagent sessions")
	}
	node, exists := owner.LiveSessionNode(sessionID)
	if !exists {
		return nil, runtime.LiveSessionNode{}, errors.New("live session not found")
	}
	sess, hasSess := owner.LiveSession(sessionID)
	if !hasSess || sess == nil {
		return nil, runtime.LiveSessionNode{}, errors.New("live session has no in-memory pointer")
	}
	return sess, node, nil
}

// StopLiveSession stops a live descendant session.
func (sm *SessionManager) StopLiveSession(_ context.Context, sessionID string) error {
	return sm.controlLiveSession(sessionID, func(rt liveSessionRuntime) error {
		return rt.StopSessionByID(sessionID)
	})
}

func (sm *SessionManager) controlLiveSession(sessionID string, do func(rt liveSessionRuntime) error) error {
	treeRt, _, err := sm.resolveLiveSession(sessionID)
	if err != nil {
		return err
	}
	return do(treeRt)
}

// apiLiveNode converts a runtime.LiveSessionNode into its wire-format counterpart.
func apiLiveNode(node runtime.LiveSessionNode) api.LiveSessionNode {
	return api.LiveSessionNode{
		SessionID:       node.SessionID,
		ParentSessionID: node.ParentSessionID,
		RootSessionID:   node.RootSessionID,
		AgentName:       node.AgentName,
		Title:           node.Title,
		Kind:            string(node.Kind),
		Depth:           node.Depth,
		Status:          node.Status,
		CreatedAt:       node.CreatedAt,
		LastUpdateAt:    node.LastUpdateAt,
		LastPreview:     node.LastPreview,
		Error:           node.Error,
	}
}

// apiLiveNodes converts a slice of runtime.LiveSessionNode into its wire-format counterpart.
func apiLiveNodes(nodes []runtime.LiveSessionNode) []api.LiveSessionNode {
	out := make([]api.LiveSessionNode, len(nodes))
	for i, n := range nodes {
		out[i] = apiLiveNode(n)
	}
	return out
}
