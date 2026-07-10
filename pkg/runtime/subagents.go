package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tools"
)

// subagentManager supervises async subagents above the per-session driver
// layer. It owns tree metadata, parent/child linkage, latest results, and
// reporting; each child session's run/wake/delivery lifecycle is owned by its
// sessionDriver.
type subagentManager struct {
	r      *LocalRuntime
	tree   *subagent.Tree
	ctx    context.Context //nolint:containedctx // long-lived context bounding detached child sessions; cancelled by Close.
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu       sync.Mutex
	sessions map[string]*sessionSubagents
	children map[subagent.NodeID]*childRecord
}

// sessionSubagents tracks the outstanding subagents of one session and the tree
// node that represents it (so subagents it spawns are linked under the right
// parent at any depth). live counts this session's own running subagents. For
// top-level sessions (synthetic root node) the session object is retained so
// tree snapshots can be mirrored onto it for persistence.
type sessionSubagents struct {
	node     subagent.NodeID
	live     int
	topLevel bool
	sess     *session.Session
}

// childRecord retains what read_subagent / send_message / reporting need about
// a subagent. The sessionDriver owns run/wake/cancel state; unwatch releases
// the manager's completion callback when the child is stopped.
type childRecord struct {
	name            string
	parentSession   string
	parentAgentName string
	sessionID       string
	session         *session.Session
	parentSess      *session.Session
	agent           *agent.Agent
	unwatch         func()

	state  subagent.NodeState
	result string
	errMsg string
}

func newSubagentManager(r *LocalRuntime) *subagentManager {
	ctx, cancel := context.WithCancel(r.ctx())
	return &subagentManager{
		r:        r,
		tree:     subagent.NewTree(),
		ctx:      ctx,
		cancel:   cancel,
		sessions: map[string]*sessionSubagents{},
		children: map[subagent.NodeID]*childRecord{},
	}
}

// Tree returns the live subagent tree for inspection/UI.
func (m *subagentManager) Tree() *subagent.Tree { return m.tree }

// ensureSessionLocked returns (creating if needed) the tracking record for a
// session. node is the tree node representing the session; when empty (a
// top-level session) a synthetic root node is created. Caller holds m.mu.
func (m *subagentManager) ensureSessionLocked(sess *session.Session, agentName string, node subagent.NodeID) *sessionSubagents {
	if st, ok := m.sessions[sess.ID]; ok {
		return st
	}
	st := &sessionSubagents{node: node}
	if node == "" {
		st.node = subagent.SessionRootID(sess.ID)
		st.topLevel = true
		st.sess = sess
		_ = m.tree.Add(subagent.Node{ID: st.node, Agent: agentName, State: subagent.NodeRunning})
	}
	m.sessions[sess.ID] = st
	return st
}

// persistSnapshot mirrors the live tree onto every top-level session (for the
// TUI's in-memory access) and writes it to the subagent store. The explicit
// store write matters: subagents change state mid-run (or while the parent is
// idle), so nothing else would flush the snapshot before the process exits.
func (m *subagentManager) persistSnapshot() {
	snap := m.tree.Snapshot()

	m.mu.Lock()
	var sessions []*session.Session
	for _, st := range m.sessions {
		if st.topLevel && st.sess != nil {
			sessions = append(sessions, st.sess)
		}
	}
	m.mu.Unlock()

	ctx := context.WithoutCancel(m.ctx)
	for _, sess := range sessions {
		sess.SetSubagentTree(&snap)
		if store := m.r.subagentStore; store != nil {
			if err := store.SaveTree(ctx, sess.ID, snap); err != nil {
				slog.WarnContext(ctx, "Failed to persist subagent tree snapshot", "session_id", sess.ID, "error", err)
			}
		}
	}
}

// Spawn starts a subagent concurrently and returns its node id immediately.
func (m *subagentManager) Spawn(parentSess *session.Session, parentAgentName string, ref subagent.AllowedSubagent, task string) subagent.NodeID {
	childID := m.tree.NewNodeID()

	childAgent, err := m.r.team.Agent(ref.Agent)
	if err != nil {
		m.mu.Lock()
		parent := m.ensureSessionLocked(parentSess, parentAgentName, "")
		_ = m.tree.Add(subagent.Node{ID: childID, Agent: ref.Agent, Name: ref.Name, Parent: parent.node, State: subagent.NodeFailed, Error: err.Error()})
		m.children[childID] = &childRecord{name: ref.DisplayName(), parentSession: parentSess.ID, state: subagent.NodeFailed, errMsg: err.Error()}
		m.mu.Unlock()
		m.persistSnapshot()
		m.deliverToParent(parentSess.ID, systemInfo(fmt.Sprintf("Subagent %q (%s) failed to start: %v", ref.DisplayName(), childID, err)))
		return childID
	}

	toolsApproved, safetyPolicy, permissions := parentSess.SafetySettings()
	cfg := SubSessionConfig{
		AgentName:      ref.Agent,
		ToolsApproved:  toolsApproved,
		SafetyPolicy:   safetyPolicy,
		Permissions:    permissions,
		NonInteractive: true,
		PinAgent:       true,
	}
	childSess := newSubSession(parentSess, cfg, childAgent)
	// No Task in the config: async subagents are persistent actors whose
	// "user" is their parent, so instead of the delegation boilerplate the
	// task IS the child's first regular user prompt — its session reads like
	// an ordinary session in attached tabs, read_subagent transcripts, and
	// the model's own context alike.
	childSess.AddMessage(session.UserMessage(task))
	// Like any other session, the child gets its title generated from its
	// first user message (the task) instead of a hardcoded label.
	m.generateChildTitle(childSess, task)

	m.mu.Lock()
	parent := m.ensureSessionLocked(parentSess, parentAgentName, "")
	parent.live++
	_ = m.tree.Add(subagent.Node{
		ID:          childID,
		Agent:       ref.Agent,
		Name:        ref.Name,
		Description: ref.Description,
		Parent:      parent.node,
		SessionID:   childSess.ID,
		Task:        task,
		State:       subagent.NodeRunning,
	})
	// Register the child session under its own node so subagents it spawns are
	// linked beneath it (correct deep-tree linkage).
	m.ensureSessionLocked(childSess, ref.Agent, childID)
	rec := &childRecord{
		name:            ref.DisplayName(),
		parentSession:   parentSess.ID,
		parentAgentName: parentAgentName,
		sessionID:       childSess.ID,
		session:         childSess,
		parentSess:      parentSess,
		agent:           childAgent,
		state:           subagent.NodeRunning,
	}
	m.children[childID] = rec
	m.mu.Unlock()

	childDriver := m.r.sessionDrivers.Get(childSess)
	rec.unwatch = childDriver.OnSettled(func() { m.reportChildSettled(childID) })

	m.persistSnapshot()
	m.wg.Go(func() {
		for range childDriver.RunOrAttach(m.ctx, childSess) {
		}
	})

	return childID
}

// generateChildTitle asynchronously titles a freshly spawned subagent session
// from its first user message, exactly like a root session's first prompt.
// The resulting SessionTitleEvent reaches attached viewers through the
// session event hub (tab bar and sidebar update live). Best-effort: title
// generation failures never affect the child's run.
func (m *subagentManager) generateChildTitle(childSess *session.Session, task string) {
	gen := m.r.TitleGenerator(m.ctx)
	if gen == nil {
		return
	}
	m.wg.Go(func() {
		title, err := gen.Generate(m.ctx, childSess.ID, []string{task})
		if err != nil || title == "" {
			slog.DebugContext(m.ctx, "Subagent title generation skipped", "session_id", childSess.ID, "error", err)
			return
		}
		// The store row may not exist yet (the child persists after its first
		// turn); UpdateSessionTitle still sets the in-memory title, which the
		// first persist then writes.
		if err := m.r.UpdateSessionTitle(m.ctx, childSess, title); err != nil {
			slog.DebugContext(m.ctx, "Subagent title not yet persistable", "session_id", childSess.ID, "error", err)
		}
		m.r.sessionEvents.Publish(childSess.ID, SessionTitle(childSess.ID, title))
	})
}

// reportChildSettled records a child driver's finished turn and notifies its
// parent when the child has no pending input left. The child remains alive and
// re-messageable until explicitly stopped.
func (m *subagentManager) reportChildSettled(childID subagent.NodeID) {
	m.mu.Lock()
	rec := m.children[childID]
	if rec == nil || rec.state == subagent.NodeStopped {
		m.mu.Unlock()
		return
	}
	rec.result = rec.session.GetLastAssistantMessageContent()
	rec.errMsg = ""
	if d, ok := m.r.sessionDrivers.Lookup(rec.sessionID); ok {
		rec.errMsg = d.LastError()
	}
	parentSess, childSess, parentAgentName, childAgent := rec.parentSess, rec.session, rec.parentAgentName, rec.agent
	state := subagent.NodeIdle
	if rec.errMsg != "" {
		state = subagent.NodeFailed
	}
	rec.state = state
	m.mu.Unlock()

	if parentSess != nil && childSess != nil {
		parentSess.AddSubSession(childSess)
		m.r.persistBackgroundSubSession(context.WithoutCancel(m.ctx), parentSess.ID, childSess)
	}
	if parentSess != nil && childSess != nil && childAgent != nil {
		parentAgent, _ := m.r.team.Agent(parentAgentName)
		m.r.executeSubagentStopHooks(context.WithoutCancel(m.ctx), parentSess, childSess, parentAgent, childAgent.Name(), childSess.GetLastAssistantMessageContent())
	}

	m.reportTurn(childID, state, rec.errMsg)
}

// reportTurn records a child's finished turn in the tree and delivers a short
// report (with a response preview) to its parent. The child stays alive and
// idle: its session driver accepts future input until explicitly stopped.
func (m *subagentManager) reportTurn(childID subagent.NodeID, state subagent.NodeState, errMsg string) {
	m.mu.Lock()
	rec := m.children[childID]
	if rec == nil {
		m.mu.Unlock()
		return
	}
	parentID, name := rec.parentSession, rec.name
	childSessionID := rec.sessionID
	detail := rec.result
	if errMsg != "" {
		detail = errMsg
	}
	m.mu.Unlock()

	_ = m.tree.Update(childID, func(n *subagent.Node) {
		n.State = state
		n.Error = errMsg
	})
	m.persistSnapshot()

	// Wait for quiet: a turn that ends while the subagent's own subagents
	// are still working is bookkeeping, not news — the parent can act on
	// nothing yet, and in deep chains reporting it would wake every ancestor
	// once per leaf event (a model turn each). Stay silent; the subagent is
	// re-woken by its own children's reports, and its next turn end
	// re-evaluates this check, so a report always bubbles up once the subtree
	// settles. Failures always report; send_message(parent) never waits.
	if state != subagent.NodeFailed && m.hasWorkingSubagents(childSessionID) {
		return
	}

	verb := "finished its turn and is awaiting further input"
	label := "Full response"
	if state == subagent.NodeFailed {
		verb = "failed; you can message it again or stop it"
		label = "Error"
	}
	msg := fmt.Sprintf("Subagent %q (%s) %s.", name, childID, verb)
	// Embed the response (or its head) so the parent can often decide without
	// a read_subagent round-trip. A trailing "[...]" marks truncation: the
	// full text requires inspection. Without it, the report carries the
	// ENTIRE response.
	if preview, truncated := subagent.PreviewText(detail, subagent.PreviewLen); preview != "" {
		if truncated {
			msg += fmt.Sprintf(" %s preview: %q", label, preview+" [...]")
		} else {
			msg += fmt.Sprintf(" %s: %q", label, preview)
		}
	}
	m.deliverToParent(parentID, systemInfo(msg))
}

// stopChild explicitly finalizes a subagent of parentID: it cancels any
// in-flight run, releases callbacks, stops its own subagents recursively, and
// decrements the parent's live count. Stopped subagents keep their record
// so read_subagent still works, but accept no further input.
func (m *subagentManager) stopChild(parentID string, id subagent.NodeID) (string, error) {
	m.mu.Lock()
	rec := m.children[id]
	if rec == nil {
		m.mu.Unlock()
		return "", fmt.Errorf("no subagent with id %q", id)
	}
	if rec.parentSession != parentID {
		m.mu.Unlock()
		return "", fmt.Errorf("subagent %q is not one of yours", id)
	}
	if rec.state == subagent.NodeStopped {
		m.mu.Unlock()
		return "", fmt.Errorf("subagent %q is already stopped", id)
	}
	m.stopLocked(rec, id)
	m.mu.Unlock()

	m.persistSnapshot()
	return rec.name, nil
}

// stopLocked marks rec stopped, cancels its driver run, releases callbacks,
// and recursively stops its own subagents. Caller holds m.mu.
func (m *subagentManager) stopLocked(rec *childRecord, id subagent.NodeID) {
	rec.state = subagent.NodeStopped
	if rec.unwatch != nil {
		rec.unwatch()
		rec.unwatch = nil
	}
	m.r.sessionDrivers.StopAll(rec.sessionID)
	if st := m.sessions[rec.parentSession]; st != nil && st.live > 0 {
		st.live--
	}
	_ = m.tree.Update(id, func(n *subagent.Node) { n.State = subagent.NodeStopped })

	for childID, child := range m.children {
		if child.parentSession == rec.sessionID && child.state != subagent.NodeStopped {
			m.stopLocked(child, childID)
		}
	}
}

// Restore adopts a persisted swarm snapshot for a reloaded session, rebuilding
// child records so its subagents stay conversational across process restarts:
// an adopted subagent idles exactly like one that just finished a turn.
// Children whose sub-session or agent can no longer be loaded are shown as
// stopped. No-op when the session is already tracked in this process (an
// in-process switch back). Returns the live snapshot after adoption.
func (m *subagentManager) Restore(ctx context.Context, sess *session.Session, snapshot subagent.Snapshot) subagent.Snapshot {
	m.mu.Lock()
	_, tracked := m.sessions[sess.ID]
	m.mu.Unlock()
	if tracked {
		return m.tree.Snapshot()
	}

	rootID := subagent.SessionRootID(sess.ID)
	for _, root := range snapshot.Nodes {
		if root.Node.ID != rootID {
			continue
		}
		m.mu.Lock()
		m.ensureSessionLocked(sess, root.Node.Agent, "")
		m.mu.Unlock()
		m.adoptChildren(ctx, sess, root.Children)
	}
	m.persistSnapshot()
	return m.tree.Snapshot()
}

// adoptChildren rebuilds child records (recursively) from persisted node
// snapshots. Previously in-flight or idle children resume as idle; nodes that
// cannot be resumed (missing sub-session, unknown agent) become stopped, as do
// their descendants.
func (m *subagentManager) adoptChildren(ctx context.Context, parentSess *session.Session, nodes []subagent.NodeSnapshot) {
	for _, snap := range nodes {
		m.adoptChild(ctx, parentSess, snap)
	}
}

// adoptChild rebuilds one child record from its persisted snapshot, then
// recurses into its descendants.
func (m *subagentManager) adoptChild(ctx context.Context, parentSess *session.Session, snap subagent.NodeSnapshot) {
	node := snap.Node
	childSess, childAgent := m.loadChild(ctx, node)

	state := node.State
	switch {
	case state == subagent.NodeStopped || state == subagent.NodeFailed:
		// Keep terminal-ish states as persisted.
	case childSess == nil:
		state = subagent.NodeStopped // not resumable in this process
	default:
		state = subagent.NodeIdle // no run is in flight after a restart
	}

	node.State = state
	_ = m.tree.Add(node)

	rec := &childRecord{
		name:          node.DisplayName(),
		parentSession: parentSess.ID,
		sessionID:     node.SessionID,
		session:       childSess,
		parentSess:    parentSess,
		agent:         childAgent,
		state:         state,
		errMsg:        node.Error,
	}
	if childSess != nil {
		rec.result = childSess.GetLastAssistantMessageContent()
	}

	m.mu.Lock()
	if state != subagent.NodeStopped && childSess != nil {
		if st := m.sessions[parentSess.ID]; st != nil {
			st.live++
		}
		// Register the child session under its own node so subagents it
		// spawns are linked beneath it.
		m.ensureSessionLocked(childSess, node.Agent, node.ID)
	}
	m.children[node.ID] = rec
	m.mu.Unlock()

	if state != subagent.NodeStopped && childSess != nil {
		childID := node.ID
		driver := m.r.sessionDrivers.Get(childSess)
		rec.unwatch = driver.OnSettled(func() { m.reportChildSettled(childID) })
	}

	if childSess != nil {
		m.adoptChildren(ctx, childSess, snap.Children)
	} else {
		m.adoptStopped(snap.Children)
	}
}

// adoptStopped records unresumable descendants for display/read only.
func (m *subagentManager) adoptStopped(nodes []subagent.NodeSnapshot) {
	for _, snap := range nodes {
		node := snap.Node
		node.State = subagent.NodeStopped
		_ = m.tree.Add(node)
		m.mu.Lock()
		m.children[node.ID] = &childRecord{name: node.DisplayName(), sessionID: node.SessionID, state: subagent.NodeStopped, errMsg: node.Error}
		m.mu.Unlock()
		m.adoptStopped(snap.Children)
	}
}

// loadChild resolves a persisted child's sub-session and agent so it can be
// resumed. It re-applies the session flags that are not persisted (agent
// pinning, non-interactive execution).
func (m *subagentManager) loadChild(ctx context.Context, node subagent.Node) (*session.Session, *agent.Agent) {
	if node.SessionID == "" || m.r.sessionStore == nil {
		return nil, nil
	}
	childAgent, err := m.r.team.Agent(node.Agent)
	if err != nil {
		return nil, nil
	}
	childSess, err := m.r.sessionStore.GetSession(ctx, node.SessionID)
	if err != nil || childSess == nil {
		return nil, nil
	}
	childSess.AgentName = node.Agent
	childSess.NonInteractive = true
	return childSess, childAgent
}

// deliver routes input to a subagent's session driver. The manager validates
// stopped/ownership state before calling this, so a known idle child may wake.
func (m *subagentManager) deliver(sessionID, content string) bool {
	return m.r.DeliverMessage(m.ctx, sessionID, content)
}

// deliverToParent routes a runtime-authored note (turn report, child
// message, spawn failure) up to a parent session. Notes must never be lost:
// when the parent driver has not been seen yet they remain buffered until the
// session appears.
func (m *subagentManager) deliverToParent(sessionID, content string) {
	m.r.deliverOrBuffer(m.ctx, sessionID, content)
}

// systemInfo wraps a runtime-authored note so the model can distinguish
// harness/system information from ordinary conversation content.
func systemInfo(body string) string {
	return subagent.WrapSystemInfo(body)
}

// nodeForSession returns the subagent node backing a child session, if any.
func (m *subagentManager) nodeForSession(sessionID string) (subagent.NodeID, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, rec := range m.children {
		if rec.sessionID == sessionID {
			return id, true
		}
	}
	return "", false
}

// sendToChild delivers a message to a subagent of parentID, returning its
// display name for attribution. Subagents stay conversational after they
// respond: any non-stopped child accepts input (an idle one is re-run). It
// errors when the id is unknown, not a child of the caller, or stopped.
func (m *subagentManager) sendToChild(parentID string, id subagent.NodeID, body string) (string, error) {
	m.mu.Lock()
	rec := m.children[id]
	if rec == nil {
		m.mu.Unlock()
		return "", fmt.Errorf("no subagent with id %q", id)
	}
	if rec.parentSession != parentID {
		m.mu.Unlock()
		return "", fmt.Errorf("subagent %q is not one of yours", id)
	}
	if rec.state == subagent.NodeStopped {
		m.mu.Unlock()
		return "", fmt.Errorf("subagent %q has been stopped; spawn a new one", id)
	}
	sessionID, name := rec.sessionID, rec.name
	rec.state = subagent.NodeRunning
	m.mu.Unlock()

	_ = m.tree.Update(id, func(n *subagent.Node) { n.State = subagent.NodeRunning })
	m.persistSnapshot()

	if !m.deliver(sessionID, body) {
		return "", fmt.Errorf("subagent %q is no longer reachable", id)
	}
	return name, nil
}

// hasWorkingSubagents reports whether a session has subagents actively
// running a turn (running/starting) — work still in flight beneath it. Idle,
// completed, failed, and stopped children don't count: they produce no
// further reports on their own.
func (m *subagentManager) hasWorkingSubagents(sessionID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, rec := range m.children {
		if rec.parentSession == sessionID &&
			(rec.state == subagent.NodeRunning || rec.state == subagent.NodeStarting) {
			return true
		}
	}
	return false
}

// HasLive reports whether a session still has subagents that were not
// explicitly stopped.
func (m *subagentManager) HasLive(sessionID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.sessions[sessionID]
	return st != nil && st.live > 0
}

// Read returns a snapshot of a subagent's record by id.
func (m *subagentManager) Read(id subagent.NodeID) (childRecord, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.children[id]
	if !ok {
		return childRecord{}, false
	}
	return *rec, true
}

// Close cancels every running subagent and waits for their goroutines to exit.
func (m *subagentManager) Close() {
	m.cancel()
	m.wg.Wait()
}

// allowedFromAgent builds the spawn allow-list for an agent from its declared
// async subagents, resolving each target's description from the team when the
// ref does not override it.
func (r *LocalRuntime) allowedFromAgent(a *agent.Agent) []subagent.AllowedSubagent {
	refs := a.AsyncSubagents()
	allowed := make([]subagent.AllowedSubagent, 0, len(refs))
	for _, ref := range refs {
		target, err := r.team.Agent(ref.Agent)
		if err != nil || target == nil {
			slog.WarnContext(r.ctx(), "Ignoring unresolved async subagent reference", "agent", a.Name(), "subagent", ref.Agent, "error", err)
			continue
		}
		desc := ref.Description
		if desc == "" {
			desc = target.Description()
		}
		allowed = append(allowed, subagent.AllowedSubagent{
			Agent:       ref.Agent,
			Name:        ref.Name,
			Description: desc,
		})
	}
	return allowed
}

// handleSpawnSubagent starts a declared subagent concurrently and returns
// immediately. The subagent's settled turns arrive as status updates carrying
// its response (or a truncated preview); the agent uses read_subagent to
// inspect details on demand.
func (r *LocalRuntime) handleSpawnSubagent(_ context.Context, sess *session.Session, tc tools.ToolCall, _ EventSink) (*tools.ToolCallResult, error) {
	var args subagent.SpawnArgs
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if strings.TrimSpace(args.Task) == "" {
		return tools.ResultError("task is required"), nil
	}

	a := r.resolveSessionAgent(sess)
	allowed := r.allowedFromAgent(a)
	ref, ok := subagent.FindAllowed(allowed, strings.TrimSpace(args.Agent))
	if !ok {
		names := make([]string, len(allowed))
		for i, x := range allowed {
			names[i] = x.DisplayName()
		}
		if len(names) == 0 {
			return tools.ResultError(fmt.Sprintf("agent %q is not one of your subagents; you have none configured", args.Agent)), nil
		}
		return tools.ResultError(fmt.Sprintf("agent %q is not one of your subagents. Available: %s", args.Agent, strings.Join(names, ", "))), nil
	}

	id := r.subagents.Spawn(sess, a.Name(), ref, args.Task)
	return tools.ResultSuccess(fmt.Sprintf(
		"Spawned subagent %q (%s). It is running concurrently and stays available for follow-ups; when it settles, a status update with its response arrives as a message (a preview ending in [...] means read_subagent has the rest). Use send_message to continue its conversation, or stop_subagent when you are done with it. Continue working or finish your response — do not poll.",
		ref.DisplayName(), id)), nil
}

// handleReadSubagent returns a subagent's status, its latest result (default),
// its last N messages (last_messages), or its full transcript (full).
func (r *LocalRuntime) handleReadSubagent(_ context.Context, _ *session.Session, tc tools.ToolCall, _ EventSink) (*tools.ToolCallResult, error) {
	var args subagent.ReadArgs
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	id := strings.TrimSpace(args.SubagentID)
	if id == "" {
		return tools.ResultError("subagent_id is required"), nil
	}
	rec, ok := r.subagents.Read(subagent.NodeID(id))
	if !ok {
		return tools.ResultError(fmt.Sprintf("no subagent with id %q", id)), nil
	}

	header := fmt.Sprintf("Subagent %q (%s) — %s", rec.name, id, rec.state)

	// Transcript modes: full or last-N. Available even while the subagent is
	// still running (partial transcript so far).
	if args.Full || args.LastMessages > 0 {
		if rec.session == nil {
			return tools.ResultSuccess(header + "\n\n(no transcript available)"), nil
		}
		limit := 0
		if !args.Full {
			limit = args.LastMessages
		}
		transcript := renderTranscript(rec.session, limit)
		if transcript == "" {
			transcript = "(no messages yet)"
		}
		return tools.ResultSuccess(header + "\n\n" + transcript), nil
	}

	// Default: the latest result (the subagent's most recent finished turn).
	switch {
	case rec.state == subagent.NodeFailed && rec.errMsg != "":
		return tools.ResultSuccess(fmt.Sprintf("%s:\n\n%s", header, rec.errMsg)), nil
	case rec.result != "":
		return tools.ResultSuccess(fmt.Sprintf("%s:\n\n%s", header, rec.result)), nil
	default:
		return tools.ResultSuccess(header + ". No result yet; pass full:true or last_messages:N to see progress."), nil
	}
}

// handleSendMessage delivers an asynchronous message to another agent: the
// reserved target "parent", or one of the caller's running subagents by id.
func (r *LocalRuntime) handleSendMessage(_ context.Context, sess *session.Session, tc tools.ToolCall, _ EventSink) (*tools.ToolCallResult, error) {
	var args subagent.SendArgs
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	to := strings.TrimSpace(args.To)
	if to == "" {
		return tools.ResultError("to is required"), nil
	}

	if to == subagent.ParentAlias {
		if sess.ParentID == "" {
			return tools.ResultError("you have no parent to message"), nil
		}
		// Stamp the sender's node id so consumers (model and TUI) can attribute
		// the message to a specific subagent, matching the turn reports.
		from := fmt.Sprintf("subagent %q", r.resolveSessionAgent(sess).Name())
		if id, ok := r.subagents.nodeForSession(sess.ID); ok {
			from += fmt.Sprintf(" (%s)", id)
		}
		body := systemInfo(fmt.Sprintf("Message from %s:\n\n%s", from, args.Message))
		r.subagents.deliverToParent(sess.ParentID, body)
		return tools.ResultSuccess("Message delivered to parent."), nil
	}

	// Parent-to-child messages are ordinary user input: the parent is the
	// child's "user", so no system_info framing — the child's session stays
	// identical in shape to a user-driven session (and viewers render it as
	// such).
	name, err := r.subagents.sendToChild(sess.ID, subagent.NodeID(to), args.Message)
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}
	return tools.ResultSuccess(fmt.Sprintf("Message delivered to subagent %q (%s).", name, to)), nil
}

// handleStopSubagent explicitly finalizes one of the caller's subagents:
// interrupts any in-flight run and dismisses it (and its own subagents). The
// transcript stays readable via read_subagent.
func (r *LocalRuntime) handleStopSubagent(_ context.Context, sess *session.Session, tc tools.ToolCall, _ EventSink) (*tools.ToolCallResult, error) {
	var args subagent.StopArgs
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	id := strings.TrimSpace(args.SubagentID)
	if id == "" {
		return tools.ResultError("subagent_id is required"), nil
	}
	name, err := r.subagents.stopChild(sess.ID, subagent.NodeID(id))
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}
	return tools.ResultSuccess(fmt.Sprintf("Stopped subagent %q (%s).", name, id)), nil
}

// renderTranscript formats a session's conversation as "role: content" lines.
// When limit > 0 only the last `limit` messages are included. System messages
// are skipped; empty messages are omitted.
func renderTranscript(sess *session.Session, limit int) string {
	msgs := sess.GetAllMessages()
	rendered := make([]string, 0, len(msgs))
	for i := range msgs {
		content := strings.TrimSpace(msgs[i].Message.Content)
		if content == "" {
			continue
		}
		rendered = append(rendered, fmt.Sprintf("%s: %s", msgs[i].Message.Role, content))
	}
	if limit > 0 && len(rendered) > limit {
		rendered = rendered[len(rendered)-limit:]
	}
	return strings.Join(rendered, "\n\n")
}

// SubagentTree returns the live async subagent swarm for this runtime, used by
// the TUI to render a running-subagent tree. Never nil.
func (r *LocalRuntime) SubagentTree() *subagent.Tree {
	return r.subagents.Tree()
}

// SubagentAttachInfo describes what a UI needs to attach a live view to an
// async subagent's sub-session.
type SubagentAttachInfo struct {
	NodeID          subagent.NodeID
	Agent           string // agent running the sub-session
	Name            string // display name (ref name or agent)
	Session         *session.Session
	ParentSessionID string
	ParentAgent     string
}

// SubagentAttachInfo resolves a subagent node id to its attach info. The
// returned session is the live object the manager drives — viewers must treat
// it as read-only. ok is false for unknown ids or subagents without a
// session (failed spawns, unresumable restores).
func (r *LocalRuntime) SubagentAttachInfo(id subagent.NodeID) (SubagentAttachInfo, bool) {
	rec, ok := r.subagents.Read(id)
	if !ok || rec.session == nil {
		return SubagentAttachInfo{}, false
	}
	info := SubagentAttachInfo{
		NodeID:          id,
		Name:            rec.name,
		Session:         rec.session,
		ParentSessionID: rec.parentSession,
		ParentAgent:     rec.parentAgentName,
	}
	if rec.agent != nil {
		info.Agent = rec.agent.Name()
	}
	if node, ok := r.subagents.tree.Node(id); ok {
		if info.Agent == "" {
			info.Agent = node.Agent
		}
		if info.ParentAgent == "" {
			if parent, ok := r.subagents.tree.Node(node.Parent); ok {
				info.ParentAgent = parent.Agent
			}
		}
	}
	return info, true
}

// SubagentNodeForSession returns the subagent node backing a child
// sub-session, when this runtime's manager tracks one for it. Lets the TUI
// resolve session links (e.g. a nested tab's "parent" link) to an attachable
// subagent even when no tab is open for that session.
func (r *LocalRuntime) SubagentNodeForSession(sessionID string) (subagent.NodeID, bool) {
	return r.subagents.nodeForSession(sessionID)
}

// RestoreSubagentTree rebuilds the subagent swarm of a reloaded session from
// the subagent store, so its subagents stay conversational across process
// restarts, and returns the resulting live snapshot (nil when the session has
// no persisted tree). Safe to call for sessions already tracked in-process.
func (r *LocalRuntime) RestoreSubagentTree(ctx context.Context, sess *session.Session) (*subagent.Snapshot, error) {
	if r.subagentStore == nil {
		return nil, nil
	}
	stored, err := r.subagentStore.LoadTree(ctx, sess.ID)
	if err != nil || stored == nil {
		return nil, err
	}
	snapshot := r.subagents.Restore(ctx, sess, *stored)
	return &snapshot, nil
}
