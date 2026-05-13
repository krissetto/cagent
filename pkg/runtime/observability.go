package runtime

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

// ErrSessionNotInTree is returned by tree-control methods when the given
// session id is unknown to this runtime.
var ErrSessionNotInTree = errors.New("session is not part of this runtime's live tree")

// SubscribeSession registers a new observer on the given session id.
// The observer will receive every subsequent event emitted on that
// session (root or subagent) until Cancel is called or ctx is cancelled.
func (r *LocalRuntime) SubscribeSession(ctx context.Context, sessionID string, buffer int) *Subscription {
	return r.eventBus.Subscribe(ctx, sessionID, buffer)
}

// LiveSessionTree returns the live tree rooted at rootSessionID as a
// *SessionTree. The tree includes the root node, all live in-memory
// subagent descendants managed by this runtime, plus any persisted
// (finalized) child sessions from the store.
//
// Returns an error only if the store access fails catastrophically. A
// missing or empty root is not an error; the tree will simply contain an
// empty or single-node result.
func (r *LocalRuntime) LiveSessionTree(rootSessionID string) (*SessionTree, error) {
	root := r.buildRootNode(rootSessionID)
	descendants := r.buildDescendantNodes(rootSessionID)
	return NewSessionTree(root, descendants), nil
}

// buildRootNode constructs the root LiveSessionNode. It prefers the live
// registry so the agentName reflects the session that is actively running.
func (r *LocalRuntime) buildRootNode(rootSessionID string) LiveSessionNode {
	node := LiveSessionNode{
		ID:            rootSessionID,
		RootSessionID: rootSessionID,
		Kind:          LiveSessionRoot,
		Depth:         0,
		Status:        "running",
	}
	if r.liveSessions != nil {
		if entry, ok := r.liveSessions.get(rootSessionID); ok {
			node.AgentName = entry.agentName
		}
	}
	if node.AgentName == "" && r.agents != nil {
		node.AgentName = r.CurrentAgentName()
	}
	return node
}

// buildDescendantNodes assembles the descendant flat slice: live subagents
// first, then persisted-but-unseen descendants from the store.
func (r *LocalRuntime) buildDescendantNodes(rootSessionID string) []LiveSessionNode {
	seen := map[string]bool{rootSessionID: true}
	var out []LiveSessionNode

	// Live in-memory subagents.
	if r.subagents != nil {
		for _, snap := range r.subagents.Descendants(rootSessionID) {
			out = append(out, subagentSnapshotToNode(rootSessionID, snap))
			seen[snap.ID] = true
		}
	}

	// Persisted descendants not already covered by the in-memory tree.
	out = append(out, r.persistedDescendants(rootSessionID, seen)...)
	return out
}

// persistedDescendants walks the session store starting from parentID,
// collecting child sessions whose IDs are not yet in seen. Each returned
// node is marked Status="closed" to indicate it is finalized.
func (r *LocalRuntime) persistedDescendants(parentID string, seen map[string]bool) []LiveSessionNode {
	if r.sessionStore == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var out []LiveSessionNode
	var walk func(pid string, depth int)
	walk = func(pid string, depth int) {
		css, ok := r.sessionStore.(session.ChildSessionStore)
		if !ok {
			return
		}
		children, err := css.GetChildSessions(ctx, pid)
		if err != nil || len(children) == 0 {
			return
		}
		for _, child := range children {
			if seen[child.ID] {
				walk(child.ID, depth+1)
				continue
			}
			seen[child.ID] = true
			out = append(out, persistedSessionToNode(child, depth+1))
			walk(child.ID, depth+1)
		}
	}
	walk(parentID, 0)
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

// subagentSnapshotToNode converts a live handle snapshot to a LiveSessionNode.
func subagentSnapshotToNode(rootID string, snap subagent.HandleSnapshot) LiveSessionNode {
	return LiveSessionNode{
		ID:            snap.ID,
		ParentID:      snap.ParentSessionID,
		RootSessionID: rootID,
		AgentName:     snap.AgentName,
		Title:         snap.Title,
		Kind:          LiveSessionSubAgent,
		Depth:         snap.Depth,
		Status:        snap.Status.String(),
		CreatedAt:     snap.CreatedAt,
		LastUpdateAt:  snap.LastUpdateAt,
		LastPreview:   snap.LastPreview,
		Error:         snap.Error,
	}
}

// persistedSessionToNode converts a store-loaded child session to a
// LiveSessionNode with Status="closed".
func persistedSessionToNode(s *session.Session, depth int) LiveSessionNode {
	return LiveSessionNode{
		ID:        s.ID,
		ParentID:  s.ParentID,
		AgentName: s.AgentName,
		Kind:      LiveSessionSubAgent,
		Depth:     depth,
		Status:    "closed",
		Title:     s.Title,
		CreatedAt: s.CreatedAt,
	}
}

// LiveChildSession returns the session for a live runtime-managed subagent.
// It is used by UI attach flows before the asynchronous persistence path has
// necessarily written the child session to the session store.
func (r *LocalRuntime) LiveChildSession(sessionID string) (*session.Session, bool) {
	if r.subagents == nil || sessionID == "" {
		return nil, false
	}
	sess, err := r.subagents.Session(sessionID)
	if err != nil || sess == nil {
		return nil, false
	}
	return sess, true
}

// resolveSessionControl routes a control operation to the appropriate
// subagent or root session, eliminating the repeated lookup pattern in
// SteerSessionByID / FollowUpSessionByID / InterruptSessionByID / etc.
// subagentOp is called when sessionID belongs to a managed subagent;
// rootOp is called when sessionID is a live root session (may be nil).
func (r *LocalRuntime) resolveSessionControl(
	sessionID string,
	subagentOp func(string) error,
	rootOp func() error,
) error {
	if sessionID == "" {
		return errors.New("session id is required")
	}
	if r.subagents != nil {
		if _, err := r.subagents.Get(sessionID); err == nil {
			return subagentOp(sessionID)
		}
	}
	if rootOp != nil && r.isLiveRootSession(sessionID) {
		return rootOp()
	}
	return ErrSessionNotInTree
}

// SteerSessionByID steers an arbitrary session in this runtime's live tree.
// Root sessions use the runtime's Steer queue. Child sessions receive the
// message as a steer-mode inbox item delivered mid-turn.
func (r *LocalRuntime) SteerSessionByID(sessionID string, msg QueuedMessage) error {
	return r.resolveSessionControl(sessionID,
		func(id string) error {
			return r.subagents.Send(id, subagent.Message{
				Content:      msg.Content,
				MultiContent: msg.MultiContent,
				Mode:         subagent.MessageModeSteer,
			})
		},
		func() error { return r.Steer(msg) },
	)
}

// FollowUpSessionByID enqueues a follow-up message for the given session.
// Root sessions use the per-runtime follow-up queue. Child sessions receive
// the message on their follow-up inbox for between-turn delivery.
func (r *LocalRuntime) FollowUpSessionByID(sessionID string, msg QueuedMessage) error {
	return r.resolveSessionControl(sessionID,
		func(id string) error {
			return r.subagents.Send(id, subagent.Message{
				Content:      msg.Content,
				MultiContent: msg.MultiContent,
				Mode:         subagent.MessageModeFollowUp,
			})
		},
		func() error { return r.FollowUp(msg) },
	)
}

// InterruptSessionByID cancels the current turn of the given subagent without
// terminating the subagent itself.
func (r *LocalRuntime) InterruptSessionByID(sessionID string) error {
	return r.resolveSessionControl(sessionID,
		func(id string) error { return r.subagents.Interrupt(id) },
		nil,
	)
}

// CloseSessionByID asks a subagent session to close cleanly at its next safe
// point.
func (r *LocalRuntime) CloseSessionByID(sessionID string) error {
	return r.resolveSessionControl(sessionID,
		func(id string) error { return r.subagents.Close(id) },
		nil,
	)
}

// StopSessionByID forcibly stops a subagent session.
func (r *LocalRuntime) StopSessionByID(sessionID string) error {
	return r.resolveSessionControl(sessionID,
		func(id string) error { return r.subagents.Stop(id) },
		nil,
	)
}

// isLiveRootSession reports whether sessionID corresponds to an actively
// running root session registered with the live session registry.
func (r *LocalRuntime) isLiveRootSession(sessionID string) bool {
	if r.liveSessions == nil || sessionID == "" {
		return false
	}
	_, ok := r.liveSessions.get(sessionID)
	return ok
}
