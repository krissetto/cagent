// Package supervisor manages agent sessions.
package supervisor

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"sync"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

// SessionKind distinguishes tabs the TUI owns directly from live attached tabs.
type SessionKind string

const (
	SessionKindOwned    SessionKind = "owned"
	SessionKindAttached SessionKind = "attached"
)

// SessionRunner represents a running or attached session tab.
type SessionRunner struct {
	// ID is the supervisor/tab identifier. In Slice 1 we keep it equal to the
	// underlying runtime/live session id for both owned and attached tabs.
	ID string
	// SessionID is the underlying session being represented. It currently
	// matches ID but is kept explicit so future slices can decouple tab identity
	// from session identity without invasive churn.
	SessionID string
	Kind      SessionKind

	App *app.App

	WorkingDir      string
	Title           string
	ParentSessionID string
	RootSessionID   string
	AgentName       string

	IsRunning    bool    // True when stream is active
	NeedsAttn    bool    // True when user attention is needed
	PendingEvent tea.Msg // Event that triggered attention (for replay on tab switch)
	cancel       context.CancelFunc
	cleanup      func()
}

// SessionSpawner is a function that creates new sessions.
// It takes a working directory and returns the app, session, and cleanup function.
type SessionSpawner func(ctx context.Context, workingDir string) (*app.App, *session.Session, func(), error)

// Supervisor manages agent sessions.
type Supervisor struct {
	mu       sync.RWMutex
	runners  map[string]*SessionRunner
	order    []string // Maintains tab order
	activeID string
	spawner  SessionSpawner
	program  *tea.Program

	// programReady is closed when SetProgram is called. Subscription goroutines
	// wait on this before consuming events so that startup events (welcome message,
	// agent info, tool info) are not silently dropped.
	programReady     chan struct{}
	programReadyOnce sync.Once
}

// New creates a new supervisor.
func New(spawner SessionSpawner) *Supervisor {
	return &Supervisor{
		runners:      make(map[string]*SessionRunner),
		spawner:      spawner,
		programReady: make(chan struct{}),
	}
}

// SetProgram sets the Bubble Tea program for sending messages.
func (s *Supervisor) SetProgram(p *tea.Program) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.program = p
	s.programReadyOnce.Do(func() {
		close(s.programReady)
	})
}

// AddSession adds an existing owned session to the supervisor.
//
// AddSession does not deduplicate by session id. Callers are expected to check
// for an existing runner (e.g. via [Supervisor.GetRunner]) before calling this
// method; inserting a duplicate would overwrite the old runner's cancel/cleanup
// callbacks and append a second entry to the tab order. See
// [Supervisor.AttachSession] for the attach-side path that does dedupe.
func (s *Supervisor) AddSession(ctx context.Context, a *app.App, sess *session.Session, workingDir string, cleanup func()) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.runners[sess.ID]; ok {
		// Defensive dedup: a repeated AddSession for the same session id would
		// silently clobber the existing runner (leaking its cancel/cleanup) and
		// append a duplicate tab to s.order, corrupting reorder/close math.
		// None of our current callers hit this path, but the invariant matters
		// for future work (e.g. if handleOpenSubAgentTab's GetRunner check
		// races with anything that inserts concurrently), so we surface it
		// loudly in debug logs rather than silently breaking tab state.
		slog.DebugContext(ctx, "Supervisor.AddSession: runner already exists for session id, keeping the existing one",
			"session_id", sess.ID,
			"existing_kind", existing.Kind)
		return existing.ID
	}

	runner := &SessionRunner{
		ID:         sess.ID,
		SessionID:  sess.ID,
		Kind:       SessionKindOwned,
		App:        a,
		WorkingDir: workingDir,
		// GetTitle is concurrency-safe; a raw sess.Title read would race with
		// background title generation for subagent-backed sessions (see
		// pkg/runtime.generateSubagentTitle).
		Title:   sess.GetTitle(),
		cleanup: cleanup,
	}

	// Create a cancellable context for this session
	sessionCtx, cancel := context.WithCancel(ctx)
	runner.cancel = cancel

	s.runners[sess.ID] = runner
	s.order = append(s.order, sess.ID)

	if s.activeID == "" {
		s.activeID = sess.ID
	}

	// Start the subscription goroutine with routing
	go s.subscribeOwnedWithRouting(sessionCtx, a, sess.ID)

	return sess.ID
}

// AttachSession registers an attached live session tab backed by a LiveEventSource.
//
// The attached runner has no *app.App; it is simply a live event subscription that
// routes incoming runtime events through the same RoutedMsg path used by owned tabs.
// If the session is already attached or owned in this supervisor, the existing
// runner is returned.
func (s *Supervisor) AttachSession(ctx context.Context, node runtime.LiveSessionNode, source runtime.LiveEventSource) (*SessionRunner, error) {
	if source == nil {
		return nil, errors.New("live event source is required")
	}
	if node.ID == "" {
		return nil, errors.New("live session id is required")
	}

	s.mu.Lock()
	if existing := s.runners[node.ID]; existing != nil {
		s.mu.Unlock()
		return existing, nil
	}

	runner := &SessionRunner{
		ID:              node.ID,
		SessionID:       node.ID,
		Kind:            SessionKindAttached,
		Title:           node.Title,
		ParentSessionID: node.ParentID,
		RootSessionID:   node.RootSessionID,
		AgentName:       node.AgentName,
	}
	if runner.Title == "" {
		runner.Title = node.AgentName
	}

	sessionCtx, cancel := context.WithCancel(ctx)
	runner.cancel = cancel

	s.runners[runner.ID] = runner
	s.order = append(s.order, runner.ID)
	if s.activeID == "" {
		s.activeID = runner.ID
	}
	captureID := runner.ID
	s.notifyTabsUpdated()
	s.mu.Unlock()

	if err := s.subscribeAttachedWithRouting(sessionCtx, source, captureID, node.ID); err != nil {
		s.mu.Lock()
		if current := s.runners[captureID]; current == runner {
			delete(s.runners, captureID)
			for i, id := range s.order {
				if id == captureID {
					s.order = append(s.order[:i], s.order[i+1:]...)
					break
				}
			}
			if s.activeID == captureID {
				if len(s.order) > 0 {
					s.activeID = s.order[0]
				} else {
					s.activeID = ""
				}
			}
			s.notifyTabsUpdated()
		}
		s.mu.Unlock()
		cancel()
		return nil, err
	}

	return runner, nil
}

// SpawnSession creates and adds a new session.
func (s *Supervisor) SpawnSession(ctx context.Context, workingDir string) (string, error) {
	if s.spawner == nil {
		return "", errors.New("session spawning is not available")
	}

	a, sess, cleanup, err := s.spawner(ctx, workingDir)
	if err != nil {
		return "", err
	}

	sessionID := s.AddSession(ctx, a, sess, workingDir, cleanup)
	return sessionID, nil
}

// subscribeOwnedWithRouting subscribes to app events and wraps them with session ID.
// It waits for the program to be set before consuming events so that startup
// events (welcome message, agent/team/tool info) are not dropped.
func (s *Supervisor) subscribeOwnedWithRouting(ctx context.Context, a *app.App, runnerID string) {
	// Wait for the program to be available before consuming any events.
	// Events are buffered in app.events, so nothing is lost during this wait.
	select {
	case <-s.programReady:
	case <-ctx.Done():
		return
	}

	send := func(msg tea.Msg) {
		// Always update supervisor state from runtime events, even when the
		// program is not yet (or ever) attached. Forwarding to the program is
		// what we gate on p != nil.
		s.handleRuntimeEvent(runnerID, msg)

		s.mu.RLock()
		p := s.program
		s.mu.RUnlock()
		if p == nil {
			return
		}

		p.Send(messages.RoutedMsg{
			SessionID: runnerID,
			Inner:     msg,
		})
	}

	// Owned sessions primarily receive events through app.SubscribeWith, which
	// reads the App's RunStream event channel. Nested subagent status changes are
	// different: the runtime publishes LiveSessionTreeChangedEvent directly to
	// ancestor sessions' live EventBus topics so observers attached at any tree
	// level can refresh their snapshots. Without this side subscription, an
	// owned root tab never sees those bus-only notifications until some later
	// action (like opening/closing a child tab) explicitly re-seeds the sidebar.
	if src := a.LiveEventSource(); src != nil {
		go s.subscribeOwnedTreeChanges(ctx, src, runnerID, send)
	}

	a.SubscribeWith(ctx, send)
}

// subscribeOwnedTreeChanges subscribes to the runtime's per-session event bus
// and forwards LiveSessionTreeChangedEvent notifications for owned sessions.
//
// app.SubscribeWith only forwards events that flow through RunStream's output
// channel. The runtime publishes LiveSessionTreeChangedEvent directly to
// ancestor sessions' EventBus topics (so observers attached at any tree level
// can refresh their snapshots), bypassing RunStream entirely. Without this
// helper, owned root tabs would never see those bus-only notifications and a
// nested subagent's status changes wouldn't appear until the user manually
// opened or closed a child tab (which re-seeds the sidebar via
// SeedSubagentsFromLiveTree on tab switch).
//
// Owned-session topics are closed at the end of each RunStream, so we
// re-attach in a loop to keep receiving notifications across subsequent
// turns. We forward ONLY LiveSessionTreeChangedEvent here so we don't
// duplicate transcript/tool/sidebar updates that already flow through the
// app.SubscribeWith path.
func (s *Supervisor) subscribeOwnedTreeChanges(ctx context.Context, src runtime.LiveEventSource, runnerID string, send func(tea.Msg)) {
	for {
		if ctx.Err() != nil {
			return
		}
		events, cancel, err := src.AttachLiveSession(ctx, runnerID)
		if err != nil {
			return
		}
		drainBusForTreeChanges(ctx, events, send)
		cancel()
	}
}

func drainBusForTreeChanges(ctx context.Context, events <-chan runtime.Event, send func(tea.Msg)) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-events:
			if !ok {
				return
			}
			if _, ok := msg.(*runtime.LiveSessionTreeChangedEvent); ok {
				send(msg)
			}
		}
	}
}

// subscribeAttachedWithRouting subscribes to live session events and routes them
// through the same RoutedMsg path used by owned sessions.
func (s *Supervisor) subscribeAttachedWithRouting(ctx context.Context, source runtime.LiveEventSource, runnerID, liveSessionID string) error {
	events, cancel, err := source.AttachLiveSession(ctx, liveSessionID)
	if err != nil {
		return err
	}

	go func() {
		defer cancel()
		select {
		case <-s.programReady:
		case <-ctx.Done():
			return
		}

		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-events:
				if !ok {
					return
				}

				s.handleRuntimeEvent(runnerID, msg)

				s.mu.RLock()
				p := s.program
				s.mu.RUnlock()
				if p == nil {
					continue
				}

				p.Send(messages.RoutedMsg{
					SessionID: runnerID,
					Inner:     msg,
				})
			}
		}
	}()

	return nil
}

// handleRuntimeEvent updates runner state based on runtime events.
func (s *Supervisor) handleRuntimeEvent(runnerID string, msg tea.Msg) {
	s.mu.Lock()
	defer s.mu.Unlock()

	runner, ok := s.runners[runnerID]
	if !ok {
		return
	}

	switch ev := msg.(type) {
	case *runtime.StreamStartedEvent:
		runner.IsRunning = true
		runner.PendingEvent = nil // New stream supersedes any stale pending event
		s.notifyTabsUpdated()

	case *runtime.StreamStoppedEvent:
		runner.IsRunning = false
		runner.PendingEvent = nil // Clear any pending attention event since stream ended
		if runner.NeedsAttn {
			runner.NeedsAttn = false
		}
		s.notifyTabsUpdated()

	case *runtime.TurnStartedEvent, *runtime.TurnEndedEvent:
		// Deliberately ignored here: tab IsRunning reflects stream/session
		// lifetime, not per-turn activity. The chat/sidebar components own the
		// finer-grained TurnStarted/TurnEnded working indicators.

	case *runtime.SessionTitleEvent:
		runner.Title = ev.Title
		s.notifyTabsUpdated()

	case *runtime.ToolCallConfirmationEvent, *runtime.MaxIterationsReachedEvent, *runtime.ElicitationRequestEvent:
		// These require user attention.
		// Slice 1 keeps the current runner-local attention model. Future slices may
		// lift this to a shared session-tree interaction registry.
		if runnerID != s.activeID {
			runner.NeedsAttn = true
			runner.PendingEvent = msg
			s.notifyTabsUpdated()
			// Ring the terminal bell to alert the user
			if p := s.program; p != nil {
				go p.Send(messages.BellMsg{})
			}
		}
	}
}

// notifyTabsUpdated sends a tabs updated message (must be called with lock held).
func (s *Supervisor) notifyTabsUpdated() {
	p := s.program
	if p == nil {
		return
	}

	tabs := s.buildTabInfoLocked()
	activeIdx := s.activeIndexLocked()

	// Send asynchronously to avoid blocking.
	// Capture p locally so the goroutine doesn't race on s.program.
	go p.Send(messages.TabsUpdatedMsg{
		Tabs:      tabs,
		ActiveIdx: activeIdx,
	})
}

// buildTabInfoLocked builds tab info (must be called with lock held).
func (s *Supervisor) buildTabInfoLocked() []messages.TabInfo {
	tabs := make([]messages.TabInfo, 0, len(s.order))
	for _, id := range s.order {
		runner := s.runners[id]
		if runner == nil {
			continue
		}

		title := runner.Title
		if title == "" {
			if runner.WorkingDir != "" {
				title = filepath.Base(runner.WorkingDir)
			} else if runner.AgentName != "" {
				title = runner.AgentName
			}
		}

		tabs = append(tabs, messages.TabInfo{
			SessionID:       id,
			Title:           title,
			IsActive:        id == s.activeID,
			IsRunning:       runner.IsRunning,
			NeedsAttention:  runner.NeedsAttn,
			Kind:            string(runner.Kind),
			ParentSessionID: runner.ParentSessionID,
			RootSessionID:   runner.RootSessionID,
			AgentName:       runner.AgentName,
		})
	}
	return tabs
}

// activeIndexLocked returns the index of the active tab (must be called with lock held).
func (s *Supervisor) activeIndexLocked() int {
	for i, id := range s.order {
		if id == s.activeID {
			return i
		}
	}
	return 0
}

// SwitchTo switches to a different session.
func (s *Supervisor) SwitchTo(sessionID string) *SessionRunner {
	s.mu.Lock()
	defer s.mu.Unlock()

	runner, ok := s.runners[sessionID]
	if !ok {
		return nil
	}

	s.activeID = sessionID
	runner.NeedsAttn = false // Clear attention flag when switching to this tab
	s.notifyTabsUpdated()

	return runner
}

// ConsumePendingEvent returns and clears the pending event for the given session.
// Returns nil if no event is pending.
func (s *Supervisor) ConsumePendingEvent(sessionID string) tea.Msg {
	s.mu.Lock()
	defer s.mu.Unlock()

	runner, ok := s.runners[sessionID]
	if !ok || runner.PendingEvent == nil {
		return nil
	}

	event := runner.PendingEvent
	runner.PendingEvent = nil
	return event
}

// SetPendingEvent stores an attention event for the given session so it can
// be replayed when the user later switches to that tab. Used to re-stash a
// background dialog's originating event when the user navigates away from
// the tab that opened it.
//
// NeedsAttention is intentionally NOT set here: the user is already aware of
// the prompt (they just chose to step away from it) and we don't want to
// flag the tab as if a brand-new event had arrived.
func (s *Supervisor) SetPendingEvent(sessionID string, event tea.Msg) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if runner, ok := s.runners[sessionID]; ok {
		runner.PendingEvent = event
	}
}

// ActiveRunner returns the currently active session runner.
func (s *Supervisor) ActiveRunner() *SessionRunner {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.runners[s.activeID]
}

// GetRunner returns the runner for the given session ID, or nil if not found.
func (s *Supervisor) GetRunner(sessionID string) *SessionRunner {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.runners[sessionID]
}

// SetRunnerTitle updates the title of the runner for the given session ID.
// It also triggers a tab update notification.
func (s *Supervisor) SetRunnerTitle(sessionID, title string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if runner, ok := s.runners[sessionID]; ok {
		runner.Title = title
		s.notifyTabsUpdated()
	}
}

// ReplaceRunnerApp replaces the app, working directory, and cleanup function
// for an existing runner. The old app's context is cancelled and its cleanup
// is run asynchronously. A new subscription goroutine is started for the new app.
// This is used when restoring a session whose working directory differs from
// the runner's current one, requiring a fresh runtime.
func (s *Supervisor) ReplaceRunnerApp(ctx context.Context, sessionID string, newApp *app.App, workingDir string, cleanup func()) {
	s.mu.Lock()
	runner, ok := s.runners[sessionID]
	if !ok {
		s.mu.Unlock()
		return
	}

	// Cancel old subscription and collect old cleanup.
	if runner.cancel != nil {
		runner.cancel()
	}
	oldCleanup := runner.cleanup

	// Replace app, working dir, and cleanup.
	runner.App = newApp
	runner.WorkingDir = workingDir
	runner.cleanup = cleanup
	runner.Kind = SessionKindOwned
	if runner.SessionID == "" {
		runner.SessionID = sessionID
	}

	// Create a new cancellable context for the replacement.
	sessionCtx, cancel := context.WithCancel(ctx)
	runner.cancel = cancel

	s.notifyTabsUpdated()
	s.mu.Unlock()

	// Run old cleanup outside the lock.
	if oldCleanup != nil {
		go oldCleanup()
	}

	// Start routing events from the new app.
	go s.subscribeOwnedWithRouting(sessionCtx, newApp, sessionID)
}

// ActiveID returns the ID of the currently active session.
func (s *Supervisor) ActiveID() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.activeID
}

// Spawner returns the session spawner function, or nil if none is configured.
func (s *Supervisor) Spawner() SessionSpawner {
	return s.spawner
}

// CloseSession closes a session and removes it from the supervisor.
func (s *Supervisor) CloseSession(sessionID string) (nextActiveID string) {
	s.mu.Lock()

	runner, ok := s.runners[sessionID]
	if !ok {
		nextActiveID = s.activeID
		s.mu.Unlock()
		return nextActiveID
	}

	// Cancel the session context / attachment subscription.
	if runner.cancel != nil {
		runner.cancel()
	}
	cleanup := runner.cleanup

	// Remove from maps
	delete(s.runners, sessionID)

	// Remove from order slice, remembering where it was.
	closedIdx := 0
	for i, id := range s.order {
		if id == sessionID {
			closedIdx = i
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}

	// If this was the active session, switch to the previous tab (or the
	// first one when closing the first tab).
	if s.activeID == sessionID {
		if len(s.order) > 0 {
			prevIdx := max(closedIdx-1, 0)
			s.activeID = s.order[prevIdx]
		} else {
			s.activeID = ""
		}
	}

	s.notifyTabsUpdated()
	nextActiveID = s.activeID
	s.mu.Unlock()

	// Run cleanup outside the lock so it can't deadlock.
	// Attached tabs have no cleanup, and cancelling them is intentionally
	// non-destructive: it only drops the subscription.
	if cleanup != nil {
		go cleanup()
	}

	return nextActiveID
}

// Count returns the number of sessions.
func (s *Supervisor) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.runners)
}

// GetTabs returns the current tab info.
func (s *Supervisor) GetTabs() ([]messages.TabInfo, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.buildTabInfoLocked(), s.activeIndexLocked()
}

// ReorderTab moves the tab at fromIdx to toIdx, shifting others accordingly.
func (s *Supervisor) ReorderTab(fromIdx, toIdx int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if fromIdx < 0 || fromIdx >= len(s.order) || toIdx < 0 || toIdx >= len(s.order) || fromIdx == toIdx {
		return
	}

	id := s.order[fromIdx]
	s.order = append(s.order[:fromIdx], s.order[fromIdx+1:]...)
	s.order = append(s.order[:toIdx], append([]string{id}, s.order[toIdx:]...)...)
	s.notifyTabsUpdated()
}

// Shutdown closes all sessions.
func (s *Supervisor) Shutdown() {
	s.mu.Lock()

	// Cancel all contexts first, then collect cleanup functions.
	var cleanups []func()
	for _, runner := range s.runners {
		if runner.cancel != nil {
			runner.cancel()
		}
		if runner.cleanup != nil {
			cleanups = append(cleanups, runner.cleanup)
		}
	}

	count := len(s.runners)
	s.runners = make(map[string]*SessionRunner)
	s.order = nil
	s.activeID = ""
	s.mu.Unlock()

	// Run cleanups outside the lock so they can't deadlock.
	for _, cleanup := range cleanups {
		cleanup()
	}

	slog.Debug("Supervisor shutdown complete", "sessions", count)
}
