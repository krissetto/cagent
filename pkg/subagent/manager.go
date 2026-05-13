package subagent

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker-agent/pkg/inbox"
	"github.com/docker/docker-agent/pkg/session"
)

// Manager owns all live subagents for a runtime instance.
//
// It tracks parent-child relationships and maintains a per-parent
// mailbox of [Envelope] updates. Parents consume envelopes either at
// safe points while still running or by blocking on the mailbox after
// their own turn completes.
//
// The Manager also enforces two recursion safety caps:
//
//   - [ManagerOption.WithMaxDepth] limits how deep the subagent tree can get
//   - [ManagerOption.WithMaxDescendants] limits how many live descendants a
//     single root-session tree can accumulate
//
// Optionally, when [WithIdleAutoFinalize] is set, the Manager runs a
// background sweeper that asks any subagent that has been waiting in
// [StatusWaiting] longer than the configured timeout to finalize cleanly.
type Manager struct {
	runner Runner

	maxDepth         int
	maxDescendants   int
	idleAutoFinalize time.Duration

	mu      sync.RWMutex
	parents map[string]*parentState
	byID    map[string]*Handle

	// envelopeListeners is invoked synchronously (in order) after every
	// envelope is enqueued onto its parent's inbox — regardless of whether
	// the immediate parent's loop ever drains it. The runtime registers
	// listeners to fan a tree-mutation notification out to every ancestor
	// session, so an observer attached to the root (or any intermediate
	// ancestor) sees nested state changes even when an intermediate parent
	// has already terminated and will never drain its own inbox.
	envelopeListeners []func(Envelope)

	// childRegisteredListeners is invoked synchronously (in order) after a
	// new child handle is registered with the manager. It mirrors
	// envelopeListeners, but for tree-creation events that do not produce
	// an envelope. Together the two listener sets cover every transition
	// observers need to learn about.
	childRegisteredListeners []func(*Handle)

	// shutdownCancel cancels background bookkeeping goroutines (e.g. the idle
	// auto-finalize sweeper). Nil when the manager has no optional
	// workers running.
	shutdownCancel context.CancelFunc
	shutdownOnce   sync.Once
}

type parentState struct {
	parentID  string
	envelopes []Envelope
	notify    inbox.Signal
}

const parentEnvelopeCap = 32

// ManagerOption configures a [Manager] at construction time.
type ManagerOption func(*Manager)

// WithMaxDepth overrides the default maximum subagent depth.
// A non-positive value restores the default ([DefaultMaxDepth]).
func WithMaxDepth(n int) ManagerOption {
	return func(m *Manager) {
		if n > 0 {
			m.maxDepth = n
		}
	}
}

// WithMaxDescendants overrides the default maximum number of live
// descendants per root-session tree. A non-positive value restores the
// default ([DefaultMaxDescendants]).
func WithMaxDescendants(n int) ManagerOption {
	return func(m *Manager) {
		if n > 0 {
			m.maxDescendants = n
		}
	}
}

// WithIdleAutoFinalize enables a background sweeper that finalizes any
// subagent still in [StatusWaiting] (i.e. has completed a turn and is
// awaiting new input) whose most recent activity is older than the
// given timeout. A non-positive value disables auto-finalization and
// keeps subagents alive until the parent explicitly finalizes or stops
// them. This is off by default on purpose: most workflows want to keep
// long-lived reviewers / planners alive indefinitely.
func WithIdleAutoFinalize(timeout time.Duration) ManagerOption {
	return func(m *Manager) {
		if timeout > 0 {
			m.idleAutoFinalize = timeout
		}
	}
}

// NewManager builds a new Manager around the provided Runner.
func NewManager(runner Runner, opts ...ManagerOption) *Manager {
	m := &Manager{
		runner:         runner,
		maxDepth:       DefaultMaxDepth,
		maxDescendants: DefaultMaxDescendants,
		parents:        make(map[string]*parentState),
		byID:           make(map[string]*Handle),
	}
	for _, opt := range opts {
		opt(m)
	}
	if m.idleAutoFinalize > 0 {
		ctx, cancel := context.WithCancel(context.Background())
		m.shutdownCancel = cancel
		go m.runIdleAutoFinalize(ctx)
	}
	return m
}

// Shutdown stops any background goroutines owned by the manager. After
// Shutdown returns, the manager remains usable for direct calls but
// optional bookkeeping (e.g. idle auto-finalize) no longer runs. Safe
// to call zero or more times.
//
// Shutdown does NOT stop live subagents. Callers that want a clean
// teardown of every running child loop must call [Manager.StopAll]
// first. Most embedders should call StopAll then Shutdown back-to-back
// from their own Close path; see [LocalRuntime.Close] for the canonical
// example.
func (m *Manager) Shutdown() {
	m.shutdownOnce.Do(func() {
		if m.shutdownCancel != nil {
			m.shutdownCancel()
		}
	})
}

// StopAll asks every live subagent to terminate and waits for each
// child loop to exit, bounded by the supplied context.
//
// StopAll is the counterpart to [Manager.StartChild]: it is the only
// operation that guarantees no orphan goroutines remain after the
// manager's owning runtime tears down. Steps:
//
//  1. Snapshot every currently-known handle under the manager lock.
//  2. Request a clean close on each (cooperative; child loops drain
//     their own state and emit a terminal envelope).
//  3. Cancel each handle's context so any in-flight provider call,
//     tool execution, or descendant cascade unwinds.
//  4. Wait for each child loop's done channel, capped by ctx.
//
// Any handle whose loop fails to exit before ctx is cancelled is
// reported in the returned error so the caller can surface a useful
// shutdown diagnostic. Subsequent calls are idempotent: terminal
// handles are skipped.
func (m *Manager) StopAll(ctx context.Context) error {
	m.mu.RLock()
	handles := make([]*Handle, 0, len(m.byID))
	for _, h := range m.byID {
		handles = append(handles, h)
	}
	m.mu.RUnlock()

	for _, h := range handles {
		if !h.IsLive() {
			continue
		}
		h.stopForcefully()
	}

	var lingering []string
	for _, h := range handles {
		done := h.LoopDone()
		if done == nil {
			// Handle never had a loop attached (possible only in
			// tests with a runner that returns nil); nothing to wait
			// on.
			continue
		}
		select {
		case <-done:
			continue
		default:
		}
		select {
		case <-done:
		case <-ctx.Done():
			lingering = append(lingering, ShortRef(h.ID()))
		}
	}
	if len(lingering) > 0 {
		return &ShutdownTimeoutError{Lingering: lingering}
	}
	return nil
}

// IdleAutoFinalizeTimeout returns the configured idle auto-finalize
// window. Zero means the feature is disabled.
func (m *Manager) IdleAutoFinalizeTimeout() time.Duration { return m.idleAutoFinalize }

// MaxDepth returns the configured depth cap.
func (m *Manager) MaxDepth() int { return m.maxDepth }

// MaxDescendants returns the configured per-tree descendant cap.
func (m *Manager) MaxDescendants() int { return m.maxDescendants }

// StartChild creates, registers, and starts a new subagent.
//
// StartChild enforces both the depth cap and the per-root-tree descendant cap
// before the child loop is started. Rejected calls never touch the Runner.
func (m *Manager) StartChild(ctx context.Context, cfg StartConfig, childSession *session.Session) (*Handle, error) {
	if cfg.Parent == nil {
		return nil, errors.New("parent session is required")
	}
	if cfg.AgentName == "" {
		return nil, errors.New("agent name is required")
	}
	if childSession == nil {
		return nil, errors.New("child session is required")
	}

	m.mu.Lock()
	parentDepth := m.depthLocked(cfg.Parent.ID)
	newDepth := parentDepth + 1
	if newDepth > m.maxDepth {
		m.mu.Unlock()
		return nil, &DepthExceededError{Limit: m.maxDepth, Attempted: newDepth}
	}

	rootID := m.rootAncestorLocked(cfg.Parent.ID)
	if m.liveDescendantsLocked(rootID) >= m.maxDescendants {
		m.mu.Unlock()
		return nil, &DescendantLimitError{Limit: m.maxDescendants}
	}

	m.parentLocked(cfg.Parent.ID)
	childCtx, cancel := context.WithCancel(ctx)
	h := newHandle(cfg, childSession, newDepth, cancel, func(env Envelope) {
		m.publishEnvelope(env)
	})
	m.byID[h.ID()] = h
	listeners := m.childRegisteredListeners
	m.mu.Unlock()

	for _, fn := range listeners {
		fn(h)
	}

	done := m.runner.StartChildLoop(childCtx, h)
	// The forwarder bridges the Runner's done channel to the pre-allocated
	// h.loopDone so StopAll always has a non-nil channel to wait on. Without
	// this, registering h in m.byID above and calling setLoopDone here would
	// be a race: StopAll could see the handle in the map, call LoopDone(), get
	// nil (because setLoopDone had not run yet), skip waiting, and return while
	// the loop goroutine is still running.
	go func() {
		if done != nil {
			<-done
		}
		close(h.loopDone)
	}()
	return h, nil
}

// Send enqueues a parent→child message on the appropriate inbox.
// Steer-mode messages ([MessageModeSteer]) are pushed onto the steer
// inbox so the child loop can drain them mid-turn; all other messages
// (follow-up / zero-value mode) go onto the regular inbox for
// between-turn delivery.
func (m *Manager) Send(id string, msg Message) error {
	h, err := m.getHandle(id)
	if err != nil {
		return err
	}
	if !h.IsLive() {
		return &ClosedError{ID: id, Status: h.Status()}
	}
	var ok bool
	if msg.Mode == MessageModeSteer {
		ok = h.steerInbox.Push(msg)
	} else {
		ok = h.inbox.Push(msg)
	}
	if !ok {
		return &ClosedError{ID: id, Status: h.Status()}
	}
	// Transition the handle to running immediately so that the live-tree
	// snapshot reports the correct status when ancestor observers refresh
	// in response to LiveSessionTreeChangedEvent. Without this, the
	// snapshot can still say "waiting" because the child's runner loop
	// hasn't woken yet to call MarkRunning itself.
	h.MarkRunning()
	h.lastUpdateAt.Store(time.Now())
	return nil
}

// Close asks a subagent to terminate cleanly after its current safe
// point. The caller should expect a [UpdateKindClosed] envelope later.
//
// Close cascades: after asking the target to close, every live descendant
// of the target is also asked to stop.
func (m *Manager) Close(id string) error {
	h, err := m.getHandle(id)
	if err != nil {
		return err
	}
	if !h.IsLive() {
		return &ClosedError{ID: id, Status: h.Status()}
	}
	h.requestClose()
	m.cascadeStopDescendants(id)
	return nil
}

// Stop forcibly cancels a subagent's loop. The caller should expect a
// [UpdateKindStopped] envelope later.
//
// Stop cascades: after cancelling the target, every live descendant is
// also cancelled.
func (m *Manager) Stop(id string) error {
	h, err := m.getHandle(id)
	if err != nil {
		return err
	}
	if !h.IsLive() {
		return &ClosedError{ID: id, Status: h.Status()}
	}
	h.stopForcefully()
	m.cascadeStopDescendants(id)
	return nil
}

// Interrupt cancels the subagent's currently-executing turn without
// terminating the subagent itself. The subagent returns to
// [StatusWaiting] and remains available for follow-up messages, new
// observers, or another explicit [Manager.Close] / [Manager.Stop].
//
// If no turn is in flight, Interrupt is a no-op. Interrupt does not
// cascade to descendants: a parent that wants to halt its subtree
// should use [Manager.Stop] or [Manager.Close] instead.
func (m *Manager) Interrupt(id string) error {
	h, err := m.getHandle(id)
	if err != nil {
		return err
	}
	if !h.IsLive() {
		return &ClosedError{ID: id, Status: h.Status()}
	}
	h.Interrupt()
	return nil
}

// CascadeStop stops the subtree rooted at id, including id itself when
// id corresponds to a known subagent. If id is a root session id (not
// present in the manager), only the descendants are stopped.
func (m *Manager) CascadeStop(id string) {
	if h, err := m.getHandle(id); err == nil && h.IsLive() {
		h.stopForcefully()
	}
	m.cascadeStopDescendants(id)
}

// Get returns a snapshot for the given subagent id.
func (m *Manager) Get(id string) (HandleSnapshot, error) {
	h, err := m.getHandle(id)
	if err != nil {
		return HandleSnapshot{}, err
	}
	return h.Snapshot(), nil
}

// ResolveChildRef resolves a model-facing subagent reference to a full child id
// within the scope of a specific parent session.
//
// Resolution rules:
//   - exact full-id matches win immediately
//   - otherwise the ref is treated as a prefix match against the parent's
//     direct children
//   - zero matches returns [NotFoundError]
//   - multiple matches returns [AmbiguousRefError]
//
// This keeps short refs human/model friendly without weakening the runtime's
// internal use of full session ids.
func (m *Manager) ResolveChildRef(parentSessionID, ref string) (string, error) {
	m.mu.RLock()
	if _, ok := m.parents[parentSessionID]; !ok {
		m.mu.RUnlock()
		return "", &NotFoundError{ID: ref}
	}

	// Exact match first, so callers may still pass full ids.
	if h, ok := m.byID[ref]; ok && h.parentSessionID == parentSessionID {
		m.mu.RUnlock()
		return h.ID(), nil
	}

	matches := make([]string, 0, 1)
	for _, h := range m.byID {
		if h.parentSessionID != parentSessionID {
			continue
		}
		if strings.HasPrefix(h.ID(), ref) {
			matches = append(matches, h.ID())
		}
	}
	m.mu.RUnlock()

	switch len(matches) {
	case 0:
		return "", &NotFoundError{ID: ref}
	case 1:
		return matches[0], nil
	default:
		candidates := disambiguatedRefs(matches)
		sort.Strings(candidates)
		return "", &AmbiguousRefError{Ref: ref, Candidates: candidates}
	}
}

// Session returns the underlying child session for inspection. Callers
// must not mutate it concurrently with a live loop except through the
// manager/runtime's own coordination.
func (m *Manager) Session(id string) (*session.Session, error) {
	h, err := m.getHandle(id)
	if err != nil {
		return nil, err
	}
	return h.Session(), nil
}

// SetTitle updates the human-readable title for the given live subagent.
// The underlying child session title remains owned by the runtime/session
// layer; this method just keeps the manager's handle snapshot in sync so
// live observability consumers (sidebar, attached tabs) see the updated title.
func (m *Manager) SetTitle(id, title string) error {
	h, err := m.getHandle(id)
	if err != nil {
		return err
	}
	h.SetTitle(title)
	return nil
}

// ListParent returns stable, sorted snapshots of all children owned by
// the given parent session.
func (m *Manager) ListParent(parentSessionID string) []HandleSnapshot {
	m.mu.RLock()
	snaps := make([]HandleSnapshot, 0, 4)
	for _, h := range m.byID {
		if h.parentSessionID == parentSessionID {
			snaps = append(snaps, h.Snapshot())
		}
	}
	m.mu.RUnlock()

	if len(snaps) == 0 {
		return nil
	}
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].CreatedAt.Before(snaps[j].CreatedAt) })
	return snaps
}

// Descendants returns snapshots of every subagent descended from the
// given session id (direct children + grandchildren + ...).
func (m *Manager) Descendants(rootSessionID string) []HandleSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []HandleSnapshot
	m.walkDescendantsLocked(rootSessionID, func(h *Handle) { out = append(out, h.Snapshot()) })
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// Depth returns the depth of the given subagent id.
// Root-children have depth 1. Returns 0 if id is unknown or is itself a
// root session.
func (m *Manager) Depth(id string) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if h, ok := m.byID[id]; ok {
		return h.Depth()
	}
	return 0
}

// Ancestors returns the chain of parent session ids for the given
// subagent id, starting at the direct parent and ending at the root
// ancestor. Returns nil if id is unknown.
func (m *Manager) Ancestors(id string) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, ok := m.byID[id]; !ok {
		return nil
	}
	var chain []string
	cur := id
	for {
		h, ok := m.byID[cur]
		if !ok {
			break
		}
		parentID := h.parentSessionID
		chain = append(chain, parentID)
		cur = parentID
	}
	return chain
}

// HasLiveChildren reports whether the parent still owns any non-terminal
// children.
func (m *Manager) HasLiveChildren(parentSessionID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, h := range m.byID {
		if h.parentSessionID == parentSessionID && h.IsLive() {
			return true
		}
	}
	return false
}

// HasInFlightChildren reports whether the parent owns any child that may
// still autonomously produce a future envelope without further parent
// input. Waiting children are excluded unless they were parked by an
// interrupted turn, already have pending inbox work queued by the parent,
// or they themselves have live descendants that may still publish updates
// upward.
//
// The recursive check is what lets a parent's waitForSubagentInbox
// correctly stay alive while a grandchild chain is still working: a
// child that completed a turn (transitioned to StatusWaiting) but whose
// own children are still running has not really gone idle from the
// parent's perspective.
func (m *Manager) HasInFlightChildren(parentSessionID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.hasInFlightDescendantsLocked(parentSessionID)
}

func (m *Manager) hasPendingParentEnvelopesLocked(parentSessionID string) bool {
	ps, ok := m.parents[parentSessionID]
	return ok && len(ps.envelopes) > 0
}

func (m *Manager) hasInFlightDescendantsLocked(parentSessionID string) bool {
	// A queued envelope for parentSessionID itself means the caller still has
	// work to consume even if every descendant has already gone waiting or
	// terminal.
	if m.hasPendingParentEnvelopesLocked(parentSessionID) {
		return true
	}

	// Only scan direct children of parentSessionID. When a direct child is
	// StatusWaiting we recurse into its descendants because a future
	// grandchild envelope can still wake it, producing output that
	// propagates up to the parent. Terminal children are skipped unless they
	// have already queued an envelope for the queried parent, which is handled
	// by the top-level pending-envelope check above.
	for _, h := range m.byID {
		if h.parentSessionID != parentSessionID {
			continue
		}
		switch h.Status() {
		case StatusStarting, StatusRunning:
			return true
		case StatusWaiting:
			if h.parked.Load() {
				return true
			}
			if h.HasPendingInbox() {
				return true
			}
			if m.hasPendingParentEnvelopesLocked(h.ID()) {
				return true
			}
			if m.hasInFlightDescendantsLocked(h.ID()) {
				return true
			}
		}
	}
	return false
}

// DrainParentInbox removes and returns every pending envelope for the
// given parent. Safe to call from the runtime loop at safe points.
//
// Drain also consumes any pending notification on the parent's signal
// channel so a later select on [ParentInboxSignal] does not fire on a
// stale buffered tick after the items slice has already been emptied.
// Without that consume, a child loop that goes idle right after a
// safe-point drain can wake spuriously, drain zero envelopes, and then
// trigger another model call with the conversation still ending on an
// assistant message — which providers like Anthropic reject as
// unsupported assistant prefill.
func (m *Manager) DrainParentInbox(parentSessionID string) []Envelope {
	m.mu.Lock()
	defer m.mu.Unlock()
	ps, ok := m.parents[parentSessionID]
	if !ok {
		return nil
	}
	// Always discard a pending tick: regardless of whether items existed
	// when this call landed, the caller is observing the inbox right now,
	// so the buffered notification has served its purpose.
	ps.notify.Consume()
	if len(ps.envelopes) == 0 {
		return nil
	}
	envs := ps.envelopes
	ps.envelopes = nil
	return envs
}

// WaitParentInbox blocks until at least one envelope is available for
// the parent or the context is cancelled. It does not remove the
// envelopes; callers must follow it with DrainParentInbox.
//
// TestOnly – this helper exists primarily for test convenience. Production
// code should select on [ParentInboxSignal] and then [DrainParentInbox].
func (m *Manager) WaitParentInbox(ctx context.Context, parentSessionID string) bool {
	for {
		m.mu.Lock()
		ps, ok := m.parents[parentSessionID]
		if !ok {
			ps = m.parentLocked(parentSessionID)
		}
		if len(ps.envelopes) > 0 {
			m.mu.Unlock()
			return true
		}
		notify := ps.notify.C()
		m.mu.Unlock()

		select {
		case <-ctx.Done():
			return false
		case <-notify:
		}
	}
}

// ParentInboxSignal returns a receive-only channel that is signalled
// every time an envelope is published for the given session. The channel
// is shared across all waiters and has a buffer of one, so a waiter may
// need to re-check with [DrainParentInbox] after receiving.
//
// This allows a running child loop to wake on its own grandchildren's
// envelopes using a single select alongside its own close/inbox channels.
func (m *Manager) ParentInboxSignal(parentSessionID string) <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.parentLocked(parentSessionID).notify.C()
}

func (m *Manager) parentLocked(parentID string) *parentState {
	if ps, ok := m.parents[parentID]; ok {
		return ps
	}
	ps := &parentState{
		parentID: parentID,
		notify:   inbox.NewSignal(),
	}
	m.parents[parentID] = ps
	return ps
}

func (m *Manager) publishEnvelope(env Envelope) {
	m.mu.Lock()
	// Status-only envelopes notify the EventBus (for sidebar refresh) but must
	// NOT be pushed into the parent inbox — doing so would wake the parent
	// agent loop as if a turn completed, which is the opposite of "silent".
	if env.Kind != UpdateKindStatusOnly {
		ps := m.parentLocked(env.ParentSessionID)
		ps.envelopes = appendParentEnvelope(ps.envelopes, env)
		ps.notify.Notify()
	}
	listeners := m.envelopeListeners
	m.mu.Unlock()
	for _, fn := range listeners {
		fn(env)
	}
}

// AddEnvelopePublishedListener appends a callback invoked synchronously after
// every envelope is published into a parent inbox. Multiple listeners are
// called in registration order. The runtime uses this to fan tree-mutation
// notifications out to every ancestor session bus, ensuring late state changes
// still reach observers attached at any ancestor level.
func (m *Manager) AddEnvelopePublishedListener(fn func(Envelope)) {
	if fn == nil {
		return
	}
	m.mu.Lock()
	m.envelopeListeners = append(m.envelopeListeners, fn)
	m.mu.Unlock()
}

// AddChildRegisteredListener appends a callback invoked synchronously after a
// new child handle has been registered with the manager. Multiple listeners
// are called in registration order. It is the counterpart to
// [AddEnvelopePublishedListener] for tree-creation events that do not produce
// an envelope (a freshly-started subagent has no envelope to publish yet).
func (m *Manager) AddChildRegisteredListener(fn func(*Handle)) {
	if fn == nil {
		return
	}
	m.mu.Lock()
	m.childRegisteredListeners = append(m.childRegisteredListeners, fn)
	m.mu.Unlock()
}

func appendParentEnvelope(envelopes []Envelope, env Envelope) []Envelope {
	// Coalesce consecutive turn-completed updates from the same subagent so a
	// chatty child cannot flood its parent's mailbox while preserving the most
	// recent preview.
	if env.Kind == UpdateKindTurnCompleted && len(envelopes) > 0 {
		last := envelopes[len(envelopes)-1]
		if last.Kind == UpdateKindTurnCompleted && last.SubAgentID == env.SubAgentID {
			envelopes[len(envelopes)-1] = env
			return envelopes
		}
	}
	envelopes = append(envelopes, env)
	if len(envelopes) <= parentEnvelopeCap {
		return envelopes
	}

	// Preserve terminal envelopes whenever possible. Drop the oldest
	// non-terminal envelope first.
	for i, existing := range envelopes {
		if !isTerminalEnvelope(existing) {
			return append(envelopes[:i], envelopes[i+1:]...)
		}
	}
	// Pathological case: everything in the buffer is terminal. Keep the most
	// recent parentEnvelopeCap entries.
	return envelopes[len(envelopes)-parentEnvelopeCap:]
}

func isTerminalEnvelope(env Envelope) bool {
	switch env.Kind {
	case UpdateKindClosed, UpdateKindStopped, UpdateKindFailed:
		return true
	default:
		return false
	}
}

// PublishEnvelope is a narrowly-scoped entry point that publishes a
// synthetic [Envelope] into the target parent session's inbox, exactly as a
// real subagent loop would. It is intended for tests and for synthetic
// observability integrations that need to inject events without owning a
// live subagent loop. Callers are responsible for populating
// env.ParentSessionID correctly; the manager does not validate that the id
// corresponds to any handle it knows about, because the root session is by
// definition unknown to the manager.
func (m *Manager) PublishEnvelope(env Envelope) {
	m.publishEnvelope(env)
}

func (m *Manager) getHandle(id string) (*Handle, error) {
	m.mu.RLock()
	h, ok := m.byID[id]
	m.mu.RUnlock()
	if !ok {
		return nil, &NotFoundError{ID: id}
	}
	return h, nil
}

// disambiguatedRefs returns the shortest unique visible refs for the given ids,
// with a floor of ShortRefLen. This is only used in ambiguity errors so we can
// tell the model how to retry with a slightly longer prefix when needed.
func disambiguatedRefs(ids []string) []string {
	refs := make([]string, len(ids))
	for i, id := range ids {
		refLen := ShortRefLen
		for {
			if refLen >= len(id) {
				refs[i] = id
				break
			}
			candidate := id[:refLen]
			unique := true
			for j, other := range ids {
				if i == j {
					continue
				}
				if len(other) >= refLen && other[:refLen] == candidate {
					unique = false
					break
				}
			}
			if unique {
				refs[i] = candidate
				break
			}
			refLen++
		}
	}
	return refs
}

// depthLocked returns the depth of the session identified by id.
// If id is not a subagent (i.e. it is a root session), returns 0.
// Must be called with m.mu held.
func (m *Manager) depthLocked(id string) int {
	if h, ok := m.byID[id]; ok {
		return h.Depth()
	}
	return 0
}

// rootAncestorLocked walks up the Handle.parentSessionID chain from id until
// it reaches a session id that is not itself a subagent (the root-session id).
// Must be called with m.mu held.
func (m *Manager) rootAncestorLocked(id string) string {
	cur := id
	for {
		h, ok := m.byID[cur]
		if !ok {
			return cur
		}
		cur = h.parentSessionID
	}
}

// liveDescendantsLocked counts every live subagent in the subtree rooted
// at rootID. Must be called with m.mu held.
func (m *Manager) liveDescendantsLocked(rootID string) int {
	var count int
	m.walkDescendantsLocked(rootID, func(h *Handle) {
		if h.IsLive() {
			count++
		}
	})
	return count
}

// walkDescendantsLockedUntil invokes fn on descendants of rootID in
// unspecified order, stopping early when fn returns false. It returns true
// when the full subtree was visited and false when traversal was stopped
// early. Descendant relationships are derived from each handle's immutable
// parentSessionID rather than a duplicated per-parent child index. Must be
// called with m.mu held.
func (m *Manager) walkDescendantsLockedUntil(rootID string, fn func(*Handle) bool) bool {
	for _, h := range m.byID {
		if h.parentSessionID != rootID {
			continue
		}
		if !fn(h) {
			return false
		}
		if !m.walkDescendantsLockedUntil(h.ID(), fn) {
			return false
		}
	}
	return true
}

// walkDescendantsLocked invokes fn on every descendant of rootID, in
// unspecified order. Descendant relationships are derived from each
// handle's immutable parentSessionID rather than a duplicated per-parent
// child index. Must be called with m.mu held.
func (m *Manager) walkDescendantsLocked(rootID string, fn func(*Handle)) {
	m.walkDescendantsLockedUntil(rootID, func(h *Handle) bool {
		fn(h)
		return true
	})
}

// cascadeStopDescendants asks every live descendant of id to stop. It
// snapshots the tree under the manager lock, then releases the lock
// before touching handles so child-loop callbacks can run without
// deadlocking.
func (m *Manager) cascadeStopDescendants(id string) {
	m.mu.RLock()
	var toStop []*Handle
	m.walkDescendantsLocked(id, func(h *Handle) {
		if h.IsLive() {
			toStop = append(toStop, h)
		}
	})
	m.mu.RUnlock()

	for _, h := range toStop {
		h.stopForcefully()
	}
}

// runIdleAutoFinalize periodically sweeps live subagents and asks the
// manager to finalize anything that has been idle (in [StatusWaiting])
// longer than the configured timeout.
//
// The sweep interval is derived from the timeout so very short
// timeouts stay responsive while long timeouts don't spin uselessly.
// We floor it at 15s to keep the amortized cost negligible regardless
// of how many subagents are live, then cap it back to the timeout
// itself so we never sleep longer than the configured idle window.
// Examples:
//   - timeout=5s   -> interval=5s   (floor would be too high, so cap wins)
//   - timeout=1m   -> interval=15s  (timeout/4 is also 15s)
//   - timeout=10m  -> interval=2m30s
func (m *Manager) runIdleAutoFinalize(ctx context.Context) {
	timeout := m.idleAutoFinalize
	interval := timeout / 4
	const floor = 15 * time.Second
	if interval < floor {
		interval = floor
	}
	if interval > timeout {
		interval = timeout
	}

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.sweepIdle(timeout)
		}
	}
}

// sweepIdle is the inner loop body of runIdleAutoFinalize, exposed as
// its own method so tests can drive it synchronously.
func (m *Manager) sweepIdle(timeout time.Duration) {
	if timeout <= 0 {
		return
	}
	now := time.Now()

	m.mu.RLock()
	var stale []string
	for id, h := range m.byID {
		if h.Status() != StatusWaiting {
			continue
		}
		if h.HasPendingInbox() {
			continue
		}
		last := h.lastUpdateAt.Load()
		if last.IsZero() {
			last = h.createdAt
		}
		if now.Sub(last) < timeout {
			continue
		}
		stale = append(stale, id)
	}
	m.mu.RUnlock()

	for _, id := range stale {
		// Best-effort finalize. Errors here mean the subagent became
		// terminal or was removed between the scan and the close call,
		// which is fine to ignore.
		_ = m.Close(id)
	}
}
