package delegation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"sync"
	"time"

	"github.com/docker/docker-agent/pkg/session"
)

const shortIDAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
const shortIDLength = 5

// DelegationRunner abstracts the runtime dependency for actually running a delegation session.
type DelegationRunner interface {
	// RunDelegation runs a child session and returns the last assistant message content.
	RunDelegation(ctx context.Context, d *Delegation, sess *session.Session) (string, error)
}

// StartParams contains the parameters for starting a new delegation.
type StartParams struct {
	AgentName       string
	Task            string
	ParentSessionID string
	ParentSession   *session.Session
	WorkingDir      string // Inherited from parent session for the child's cwd
}

// OnCompletionFunc is called by the Manager when a background delegation finishes
// (successfully or not). It is always called, even for cancelled delegations.
type OnCompletionFunc func(d *Delegation, reply string, err error)

// Manager owns all delegations and provides lifecycle management.
type Manager struct {
	mu           sync.RWMutex
	delegations  map[string]*Delegation // keyed by short ID
	runner       DelegationRunner
	sessionStore session.Store
	onCompletion OnCompletionFunc

	// maxTerminalAge is how long a terminal delegation stays in the map.
	// Defaults to 30 minutes. Set via WithMaxTerminalAge.
	maxTerminalAge time.Duration

	// wg tracks in-flight background goroutines so StopAll can wait for them.
	wg sync.WaitGroup
}

// ManagerOption configures the Manager.
type ManagerOption func(*Manager)

// WithMaxTerminalAge sets the maximum age for terminal delegations before they
// are evicted from the Manager's map. Defaults to 30 minutes.
func WithMaxTerminalAge(d time.Duration) ManagerOption {
	return func(m *Manager) { m.maxTerminalAge = d }
}

// WithSessionStore sets the session store for loading persisted child sessions.
func WithSessionStore(store session.Store) ManagerOption {
	return func(m *Manager) { m.sessionStore = store }
}

// WithOnCompletion registers a callback that is invoked each time a background
// delegation goroutine finishes (success, failure, or cancellation).
// The callback is invoked synchronously from the goroutine, so it must not block.
func WithOnCompletion(fn OnCompletionFunc) ManagerOption {
	return func(m *Manager) { m.onCompletion = fn }
}

// NewManager creates a new delegation Manager with the given runner and options.
func NewManager(runner DelegationRunner, opts ...ManagerOption) *Manager {
	m := &Manager{
		delegations:    make(map[string]*Delegation),
		runner:         runner,
		maxTerminalAge: 30 * time.Minute,
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// StartEvictionLoop launches the background goroutine that removes old
// terminal delegations from the map. ctx should be cancelled when the
// manager is shut down (e.g. after StopAll returns).
// Calling StartEvictionLoop is optional; without it delegations persist in
// memory until the process exits or Evict is called explicitly.
func (m *Manager) StartEvictionLoop(ctx context.Context) {
	interval := m.maxTerminalAge / 2
	const maxInterval = 5 * time.Minute
	if interval > maxInterval {
		interval = maxInterval
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				m.evictOldTerminal()
			case <-ctx.Done():
				return
			}
		}
	}()
}

// evictOldTerminal removes delegations that are in a terminal state and
// have been terminal for longer than maxTerminalAge.
func (m *Manager) evictOldTerminal() {
	cutoff := time.Now().Add(-m.maxTerminalAge)

	// Collect IDs to evict under RLock.
	m.mu.RLock()
	var toEvict []string
	for id, d := range m.delegations {
		st := d.LoadStatus()
		if st != StatusCompleted && st != StatusFailed && st != StatusCancelled {
			continue
		}
		d.mu.Lock()
		termAt := d.TerminatedAt
		d.mu.Unlock()
		if !termAt.IsZero() && termAt.Before(cutoff) {
			toEvict = append(toEvict, id)
		}
	}
	m.mu.RUnlock()

	if len(toEvict) == 0 {
		return
	}

	// Remove under WLock.
	m.mu.Lock()
	for _, id := range toEvict {
		d, ok := m.delegations[id]
		if !ok {
			continue
		}
		// Double-check: never evict a running delegation.
		if d.LoadStatus() == StatusRunning {
			continue
		}
		delete(m.delegations, id)
		slog.Debug("Evicted terminal delegation", "delegation_id", id)
	}
	m.mu.Unlock()
}

// Evict removes a terminal delegation from the map by its short ID.
// Returns true if the delegation was found and evicted, false if it was
// not found or is still running.
func (m *Manager) Evict(shortID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.delegations[shortID]
	if !ok {
		return false
	}
	if d.LoadStatus() == StatusRunning {
		return false
	}
	delete(m.delegations, shortID)
	return true
}

// Start creates a new child session, registers the delegation, and launches a
// background goroutine to run it. It returns immediately with the delegation ID
// and child session ID — the reply (if any) is available later via DoneCh /
// GetLastReply(), and via the OnCompletionFunc callback if configured.
func (m *Manager) Start(ctx context.Context, params StartParams) (delegationID, sessionID string, err error) {
	if params.AgentName == "" {
		return "", "", fmt.Errorf("agent name is required")
	}
	if params.Task == "" {
		return "", "", fmt.Errorf("message is required")
	}

	// Create child session with parent ID and agent name
	childSess := session.New(
		session.WithParentID(params.ParentSessionID),
		session.WithAgentName(params.AgentName),
		session.WithUserMessage(params.Task),
		session.WithWorkingDir(params.WorkingDir),
		session.WithToolsApproved(true),
		session.WithSendUserMessage(false),
	)

	d := NewDelegation(childSess.ID, params.ParentSessionID, params.AgentName)
	d.Task = params.Task
	d.StoreStatus(StatusRunning)

	// Create a background context that outlives the caller's context so the
	// delegation can run asynchronously. The per-delegation cancel lets
	// Stop() / StopAll() interrupt it cleanly.
	bgCtx, cancel := context.WithCancel(context.Background())
	// Tag the context as a background delegation run so RunStream won't touch
	// the global elicitation-events channel (prevents concurrent child runs
	// from clobbering each other's elicitation routing).
	bgCtx = context.WithValue(bgCtx, BackgroundRunKey{}, true)
	d.Cancel = cancel

	// INVARIANT: wg.Add(1) must happen before inserting into m.delegations,
	// so that StopAll (which reads m.delegations then calls wg.Wait) can
	// never observe a delegation that isn't yet tracked in the waitgroup.
	m.wg.Add(1)

	m.mu.Lock()
	shortID := m.generateShortID()
	d.ID = shortID
	m.delegations[shortID] = d
	m.mu.Unlock()
	go func() {
		defer m.wg.Done()
		defer close(d.DoneCh)

		slog.Debug("Starting delegation background run",
			"delegation_id", shortID, "session_id", d.SessionID, "agent", params.AgentName)

		reply, runErr := m.runner.RunDelegation(bgCtx, d, childSess)

		if runErr != nil {
			// If the delegation was explicitly stopped, keep StatusCancelled
			// rather than overwriting it with StatusFailed.
			if !errors.Is(runErr, context.Canceled) || d.LoadStatus() != StatusCancelled {
				d.SetError(runErr)
				d.StoreStatus(StatusFailed)
			} else {
				d.StoreStatus(StatusCancelled)
			}
			if m.onCompletion != nil {
				m.onCompletion(d, "", runErr)
			}
			return
		}

		if m.sessionStore != nil {
			if err := m.sessionStore.UpdateSession(bgCtx, childSess); err != nil {
				slog.Warn("Failed to persist child session after delegation",
					"session_id", childSess.ID, "error", err)
			}
		}

		d.SetLastReply(reply)
		d.StoreStatus(StatusCompleted)
		if m.onCompletion != nil {
			m.onCompletion(d, reply, nil)
		}
	}()

	return d.ID, childSess.ID, nil
}

// Continue sends a follow-up message to an existing delegation session.
// Like Start, it is asynchronous: it launches a background goroutine and
// returns immediately. The reply (if any) is delivered via the OnCompletionFunc
// callback and available through GetLastReply() / GetDoneCh().
func (m *Manager) Continue(ctx context.Context, shortID string, message string) error {
	m.mu.RLock()
	d, ok := m.delegations[shortID]
	m.mu.RUnlock()

	if !ok {
		return fmt.Errorf("delegation not found: %s", shortID)
	}

	if message == "" {
		return fmt.Errorf("message is required")
	}

	bgCtx, cancel := context.WithCancel(context.Background())
	bgCtx = context.WithValue(bgCtx, BackgroundRunKey{}, true)

	// INVARIANT: wg.Add(1) before goroutine launch so StopAll never misses it.
	m.wg.Add(1)

	go func() {
		defer m.wg.Done()

		// If a run is currently active (initial Start or previous Continue),
		// wait for it to complete. This preserves message ordering for
		// sequential continue_delegation calls.
		if d.LoadStatus() == StatusRunning {
			doneCh := d.GetDoneCh()
			select {
			case <-doneCh:
				// Current run finished; proceed.
			case <-ctx.Done():
				// Caller context cancelled (e.g. parent stream stopped).
				cancel()
				return
			case <-bgCtx.Done():
				return
			}
		}

		// If the delegation was stopped during the wait, don't continue.
		if d.LoadStatus() == StatusCancelled {
			cancel()
			return
		}

		// Load the existing child session from the store so we preserve
		// full conversation history from previous turns.
		var childSess *session.Session
		if m.sessionStore != nil {
			if existing, err := m.sessionStore.GetSession(bgCtx, d.SessionID); err == nil && existing != nil {
				childSess = existing
				childSess.AddMessage(session.UserMessage(message))
			} else {
				slog.Warn("Failed to load existing child session for continuation, creating new",
					"session_id", d.SessionID, "error", err)
			}
		}

		// Fallback: create a new session if loading from store failed.
		if childSess == nil {
			childSess = session.New(
				session.WithParentID(d.ParentSessionID),
				session.WithAgentName(d.AgentName),
				session.WithUserMessage(message),
				session.WithToolsApproved(true),
				session.WithSendUserMessage(false),
			)
			childSess.ID = d.SessionID
		}

		// Atomically set up the new run under d.mu.
		d.mu.Lock()
		if d.LoadStatus() == StatusRunning {
			// Two concurrent Continue goroutines raced; the second loses.
			d.mu.Unlock()
			cancel()
			slog.Debug("Continue: delegation already running after wait, message dropped",
				"delegation_id", shortID)
			return
		}
		doneCh := make(chan struct{})
		d.DoneCh = doneCh
		d.Cancel = cancel
		d.StoreStatus(StatusRunning)
		d.mu.Unlock()

		defer close(doneCh)

		slog.Debug("Continuing delegation asynchronously",
			"delegation_id", shortID, "session_id", d.SessionID, "agent", d.AgentName)

		reply, runErr := m.runner.RunDelegation(bgCtx, d, childSess)

		if runErr != nil {
			if !errors.Is(runErr, context.Canceled) || d.LoadStatus() != StatusCancelled {
				d.SetError(runErr)
				d.StoreStatus(StatusFailed)
			} else {
				d.StoreStatus(StatusCancelled)
			}
			if m.onCompletion != nil {
				m.onCompletion(d, "", runErr)
			}
			return
		}

		if m.sessionStore != nil {
			if err := m.sessionStore.UpdateSession(bgCtx, childSess); err != nil {
				slog.Warn("Failed to persist child session after continuation",
					"session_id", childSess.ID, "error", err)
			}
		}

		d.SetLastReply(reply)
		d.StoreStatus(StatusCompleted)
		if m.onCompletion != nil {
			m.onCompletion(d, reply, nil)
		}
	}()

	return nil
}

// Stop cancels a running delegation
func (m *Manager) Stop(ctx context.Context, shortID string) error {
	m.mu.RLock()
	d, ok := m.delegations[shortID]
	m.mu.RUnlock()

	if !ok {
		return fmt.Errorf("delegation not found: %s", shortID)
	}

	if !d.CompareAndSwapStatus(StatusRunning, StatusCancelled) {
		current := d.LoadStatus()
		return fmt.Errorf("delegation %s is not running (status: %s)", shortID, current)
	}

	if d.Cancel != nil {
		d.Cancel()
	}

	slog.Debug("Delegation stop requested", "delegation_id", shortID)
	return nil
}

// Get returns a delegation by short ID
func (m *Manager) Get(shortID string) (*Delegation, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	d, ok := m.delegations[shortID]
	return d, ok
}

// generateShortID produces a 5-char lowercase alphanumeric ID.
// Must be called with m.mu held.
func (m *Manager) generateShortID() string {
	for {
		b := make([]byte, shortIDLength)
		for i := range b {
			b[i] = shortIDAlphabet[rand.Intn(len(shortIDAlphabet))]
		}
		id := string(b)
		if _, exists := m.delegations[id]; !exists {
			return id
		}
	}
}

// StopAll cancels all running delegations and waits for their background
// goroutines to finish. Safe to call from Close/shutdown paths.
func (m *Manager) StopAll() {
	m.mu.RLock()
	for _, d := range m.delegations {
		if d.CompareAndSwapStatus(StatusRunning, StatusCancelled) {
			if d.Cancel != nil {
				d.Cancel()
			}
		}
	}
	m.mu.RUnlock()

	// Wait for all in-flight goroutines to exit.
	m.wg.Wait()
}
