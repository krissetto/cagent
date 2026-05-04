package runtime

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

// LiveSessionNodeKind distinguishes root sessions from subagent sessions.
type LiveSessionNodeKind string

const (
	LiveSessionRoot     LiveSessionNodeKind = "root"
	LiveSessionSubAgent LiveSessionNodeKind = "subagent"
)

// LiveSessionNode is a snapshot entry describing a session in the live
// runtime tree. It is intentionally client-oriented — it carries only
// serializable, stable fields.
type LiveSessionNode struct {
	SessionID       string              `json:"session_id"`
	ParentSessionID string              `json:"parent_session_id,omitempty"`
	RootSessionID   string              `json:"root_session_id"`
	AgentName       string              `json:"agent_name"`
	Title           string              `json:"title,omitempty"`
	Kind            LiveSessionNodeKind `json:"kind"`
	Depth           int                 `json:"depth"`
	Status          string              `json:"status"`
	CreatedAt       time.Time           `json:"created_at"`
	LastUpdateAt    time.Time           `json:"last_update_at"`
	LastPreview     string              `json:"last_preview,omitempty"`
	Error           string              `json:"error,omitempty"`
}

// ErrSessionNotInTree is returned by tree-control methods when the given
// session id is unknown to this runtime.
var ErrSessionNotInTree = errors.New("session is not part of this runtime's live tree")

// SubscribeSession registers a new observer on the given session id.
// The observer will receive every subsequent event emitted on that
// session (root or subagent) until Cancel is called or ctx is cancelled.
//
// Callers that need the pre-existing transcript should load it through
// the session store separately; the subscription only streams live
// events.
func (r *LocalRuntime) SubscribeSession(ctx context.Context, sessionID string, buffer int) *Subscription {
	return r.eventBus.Subscribe(ctx, sessionID, buffer)
}

// publishSessionEvent is an internal helper that publishes ev to the
// runtime's event bus for the given session id. It is safe to call with
// a nil bus (no-op) so tests / legacy paths that skip NewLocalRuntime
// do not panic.
func (r *LocalRuntime) publishSessionEvent(sessionID string, ev Event) {
	if r.eventBus == nil || sessionID == "" || ev == nil {
		return
	}
	r.eventBus.Publish(sessionID, ev)
}

// LiveSessionTree returns the live tree rooted at the given session id,
// including the root node itself (when the runtime owns that root
// session) plus every live subagent descended from it.
//
// When a [session.Store] is configured the tree is also augmented with
// historical (finalized) descendants persisted from previous runs that are
// no longer in the in-memory subagent manager. These nodes have
// [LiveSessionNode.Status] set to "closed" so callers can distinguish them
// from currently-live entries and treat them as read-only.
//
// When the runtime does not own the given root session, the tree only
// contains the subagent nodes the runtime knows about under that id.
// This is useful when the caller (e.g. the SessionManager) stitches
// together trees across several root-session runtimes.
func (r *LocalRuntime) LiveSessionTree(rootSessionID string) []LiveSessionNode {
	rootNode := r.rootLiveNode(rootSessionID)
	allNodes := make([]LiveSessionNode, 0, 4)
	seen := map[string]bool{rootSessionID: true}
	for _, snap := range r.subagents.Descendants(rootSessionID) {
		allNodes = append(allNodes, subagentNodeToLive(rootSessionID, snap))
		seen[snap.ID] = true
	}
	// Augment with persisted descendants so reloaded sessions still show their
	// historical subagent tree in the sidebar even though the in-memory
	// manager is empty after a restart.
	allNodes = append(allNodes, r.persistedDescendantsLive(rootSessionID, seen)...)
	return NewSessionTree(rootNode, allNodes).Slice()
}

// persistedDescendantsLive walks the session store starting from rootID and
// collects every persisted descendant whose session ID is not already in seen.
// The returned nodes are marked [LiveSessionNode.Kind] LiveSessionSubAgent
// with [LiveSessionNode.Status] = "closed" so the UI can render them as
// read-only/finalized rows.
func (r *LocalRuntime) persistedDescendantsLive(rootID string, seen map[string]bool) []LiveSessionNode {
	if r.sessionStore == nil || rootID == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var out []LiveSessionNode
	var walk func(parentID string, depth int)
	walk = func(parentID string, depth int) {
		children, err := r.sessionStore.GetChildSessions(ctx, parentID)
		if err != nil || len(children) == 0 {
			return
		}
		for _, child := range children {
			if seen[child.ID] {
				walk(child.ID, depth+1)
				continue
			}
			seen[child.ID] = true
			out = append(out, persistedSessionToLive(rootID, child, depth+1))
			walk(child.ID, depth+1)
		}
	}
	walk(rootID, 0)
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

// persistedSessionToLive converts a persisted child session into a
// LiveSessionNode marked as a finalized subagent. Persisted sessions have no
// live status field, so we report them as "closed"; UI code applies any
// display-only agent-name fallback at render time.
func persistedSessionToLive(rootID string, sess *session.Session, depth int) LiveSessionNode {
	return LiveSessionNode{
		SessionID:       sess.ID,
		ParentSessionID: sess.ParentID,
		RootSessionID:   rootID,
		AgentName:       sess.AgentName,
		Title:           sess.GetTitle(),
		Kind:            LiveSessionSubAgent,
		Depth:           depth,
		Status:          "closed",
		CreatedAt:       sess.CreatedAt,
	}
}

// rootLiveNode returns the live node for the runtime's root session id.
// It prefers the [liveSessionRegistry] entry so the agent name reflects
// the agent actually pinned to the running session, falling back to the
// runtime's mutable [LocalRuntime.CurrentAgentName] only when no engine
// is currently registered.
func (r *LocalRuntime) rootLiveNode(rootSessionID string) LiveSessionNode {
	node := LiveSessionNode{
		SessionID:     rootSessionID,
		RootSessionID: rootSessionID,
		Kind:          LiveSessionRoot,
		Depth:         0,
		Status:        "running",
	}
	if r.liveSessions != nil {
		if entry, ok := r.liveSessions.get(rootSessionID); ok {
			node.AgentName = entry.agentName
			node.CreatedAt = entry.createdAt
			if entry.sess != nil {
				node.Title = entry.sess.GetTitle()
			}
			return node
		}
	}
	// Fallback: the runtime is not currently driving an engine for this
	// root id (e.g. the caller is asking about a session that lives in a
	// different runtime, or asking before RunStream has started). Use the
	// runtime's current agent selection as a best-effort label.
	node.AgentName = r.CurrentAgentName()
	return node
}

// LiveSessionNode returns a snapshot for the given session id. It
// returns ok=false when the id matches neither a known subagent nor an
// identifier the caller can reasonably treat as a root session.
//
// On miss in the in-memory live tree it falls back to the persistent
// session store so historical (finalized) subagent rows can still be
// resolved after a reload. Persisted nodes are marked Status="closed".
func (r *LocalRuntime) LiveSessionNode(sessionID string) (LiveSessionNode, bool) {
	if snap, err := r.subagents.Get(sessionID); err == nil {
		rootID := sessionID
		if anc := r.subagents.Ancestors(sessionID); len(anc) > 0 {
			rootID = anc[len(anc)-1]
		}
		return subagentNodeToLive(rootID, snap), true
	}
	// Also resolve root sessions that have a registered engine. This lets
	// callers ask the runtime about its own root session without first
	// knowing it is the root.
	if r.liveSessions != nil {
		if entry, ok := r.liveSessions.get(sessionID); ok && entry.kind == LiveSessionRoot {
			return r.rootLiveNode(sessionID), true
		}
	}
	// Persistent fallback: a finalized subagent row from a previous run.
	if r.sessionStore != nil && sessionID != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if sess, err := r.sessionStore.GetSession(ctx, sessionID); err == nil && sess != nil && sess.IsSubSession() {
			rootID := r.persistedRootID(ctx, sess)
			return persistedSessionToLive(rootID, sess, 0), true
		}
	}
	return LiveSessionNode{}, false
}

// persistedRootID walks parent_id links upward to find the root ancestor for
// the given persisted session. Returns the session's own ID if traversal
// fails or if any link is missing.
func (r *LocalRuntime) persistedRootID(ctx context.Context, sess *session.Session) string {
	if sess == nil {
		return ""
	}
	current := sess
	for i := 0; i < 32 && current.ParentID != ""; i++ {
		parent, err := r.sessionStore.GetSession(ctx, current.ParentID)
		if err != nil || parent == nil {
			return current.ParentID
		}
		current = parent
	}
	return current.ID
}

// LiveSession returns the in-memory [*session.Session] for a live session
// managed by this runtime. Callers get a direct pointer so they can render
// history, subscribe to the session's event bus, and observe mutations made
// by the loop without copying state.
//
// When the in-memory live tree does not have the session, this falls back to
// the persistent session store so reloaded callers can still read historical
// (finalized) subagent transcripts. The returned session is a fresh load
// from the store and should be treated as read-only — no live engine is
// driving it.
//
// Returns ok=false when the id is unknown.
func (r *LocalRuntime) LiveSession(sessionID string) (*session.Session, bool) {
	if r.liveSessions != nil {
		if entry, ok := r.liveSessions.get(sessionID); ok && entry.sess != nil {
			return entry.sess, true
		}
	}
	if sess, err := r.subagents.Session(sessionID); err == nil {
		return sess, true
	}
	if r.sessionStore != nil && sessionID != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if sess, err := r.sessionStore.GetSession(ctx, sessionID); err == nil && sess != nil {
			return sess, true
		}
	}
	return nil, false
}

// SteerSessionByID steers an arbitrary session in this runtime's live
// tree. Root sessions are forwarded to the runtime's steer queue. Child
// sessions receive the message on their dedicated steer inbox
// ([subagent.MessageModeSteer]) so it is delivered mid-turn at the next
// safe point, matching the user-facing Steer API. Historical (finalized,
// store-only) sessions return [ErrSessionNotInTree] because they have no
// live target; the caller should surface a read-only error.
func (r *LocalRuntime) SteerSessionByID(sessionID string, msg QueuedMessage) error {
	if sessionID == "" {
		return errors.New("session id is required")
	}
	if _, err := r.subagents.Get(sessionID); err == nil {
		return r.subagents.Send(sessionID, subagent.Message{
			Content:      msg.Content,
			MultiContent: msg.MultiContent,
			Mode:         subagent.MessageModeSteer,
		})
	}
	if r.isLiveRootSession(sessionID) {
		return r.Steer(msg)
	}
	return ErrSessionNotInTree
}

// FollowUpSessionByID enqueues a follow-up message for the given session.
// Root sessions use the per-runtime follow-up queue, where each follow-up
// gets its own undivided turn. Child sessions use their follow-up inbox
// ([subagent.MessageModeFollowUp]) so the message is delivered between
// turns rather than mid-stream. Historical (finalized, store-only)
// sessions return [ErrSessionNotInTree].
func (r *LocalRuntime) FollowUpSessionByID(sessionID string, msg QueuedMessage) error {
	if sessionID == "" {
		return errors.New("session id is required")
	}
	if _, err := r.subagents.Get(sessionID); err == nil {
		return r.subagents.Send(sessionID, subagent.Message{
			Content:      msg.Content,
			MultiContent: msg.MultiContent,
			Mode:         subagent.MessageModeFollowUp,
		})
	}
	if r.isLiveRootSession(sessionID) {
		return r.FollowUp(msg)
	}
	return ErrSessionNotInTree
}

// isLiveRootSession reports whether sessionID corresponds to a root
// session whose engine is currently registered with this runtime. Used to
// gate the SteerSessionByID/FollowUpSessionByID fallback that targets the
// runtime's per-session steer/follow-up queues, so historical sessions
// (loaded from the store but not driven by an engine) cannot accidentally
// inject into the wrong queue.
func (r *LocalRuntime) isLiveRootSession(sessionID string) bool {
	if r.liveSessions == nil {
		return false
	}
	entry, ok := r.liveSessions.get(sessionID)
	return ok && entry.kind == LiveSessionRoot
}

// CloseSessionByID asks a subagent session to close cleanly. Root
// sessions cannot be "closed" through this path; use the session
// manager's delete/stop paths instead.
func (r *LocalRuntime) CloseSessionByID(sessionID string) error {
	if _, err := r.subagents.Get(sessionID); err == nil {
		return r.subagents.Close(sessionID)
	}
	return ErrSessionNotInTree
}

// StopSessionByID forcibly stops a subagent session in this runtime's
// tree.
func (r *LocalRuntime) StopSessionByID(sessionID string) error {
	if _, err := r.subagents.Get(sessionID); err == nil {
		return r.subagents.Stop(sessionID)
	}
	return ErrSessionNotInTree
}

// InterruptSessionByID cancels the currently-running turn of a subagent
// session in this runtime's tree without terminating the session itself.
// The child returns to a waiting state so the user can intervene (send a
// new message, observe, or follow up with [CloseSessionByID] / [StopSessionByID]).
// Returns [ErrSessionNotInTree] when the id is not a descendant subagent;
// interrupting a root session is not supported through this entry point.
func (r *LocalRuntime) InterruptSessionByID(sessionID string) error {
	if _, err := r.subagents.Get(sessionID); err == nil {
		return r.subagents.Interrupt(sessionID)
	}
	return ErrSessionNotInTree
}

func subagentNodeToLive(rootID string, snap subagent.HandleSnapshot) LiveSessionNode {
	return LiveSessionNode{
		SessionID:       snap.ID,
		ParentSessionID: snap.ParentSessionID,
		RootSessionID:   rootID,
		AgentName:       snap.AgentName,
		Title:           snap.Title,
		Kind:            LiveSessionSubAgent,
		Depth:           snap.Depth,
		Status:          snap.Status.String(),
		CreatedAt:       snap.CreatedAt,
		LastUpdateAt:    snap.LastUpdateAt,
		LastPreview:     snap.LastPreview,
		Error:           snap.Error,
	}
}

// Compile-time interface checks.
var (
	_ SessionObserverSubscriber = (*LocalRuntime)(nil)
	_ SessionTreeProvider       = (*LocalRuntime)(nil)
)
