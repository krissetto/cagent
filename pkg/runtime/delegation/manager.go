package delegation

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"sync"

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
}

// Manager owns all delegations and provides lifecycle management.
type Manager struct {
	mu           sync.RWMutex
	delegations  map[string]*Delegation // keyed by short ID
	runner       DelegationRunner
	sessionStore session.Store
}

// ManagerOption configures the Manager.
type ManagerOption func(*Manager)

// WithSessionStore sets the session store for loading persisted child sessions.
func WithSessionStore(store session.Store) ManagerOption {
	return func(m *Manager) { m.sessionStore = store }
}

// NewManager creates a new delegation Manager with the given runner and options.
func NewManager(runner DelegationRunner, opts ...ManagerOption) *Manager {
	m := &Manager{
		delegations: make(map[string]*Delegation),
		runner:      runner,
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Start creates a new child session, runs the delegation synchronously, and returns
// the delegation ID (== child session ID) and the first reply from the child.
func (m *Manager) Start(ctx context.Context, params StartParams) (delegationID string, firstReply string, err error) {
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
		session.WithToolsApproved(true),
		session.WithSendUserMessage(false),
	)

	d := NewDelegation(childSess.ID, params.ParentSessionID, params.AgentName)
	d.StoreStatus(StatusRunning)

	m.mu.Lock()
	shortID := m.generateShortID()
	d.ID = shortID
	m.delegations[shortID] = d
	m.mu.Unlock()

	slog.Debug("Starting delegation", "delegation_id", shortID, "session_id", d.SessionID, "agent", params.AgentName)

	reply, runErr := m.runner.RunDelegation(ctx, d, childSess)
	if runErr != nil {
		d.SetError(runErr)
		d.StoreStatus(StatusFailed)
		close(d.DoneCh)
		return d.ID, "", runErr
	}

	if m.sessionStore != nil {
		if err := m.sessionStore.UpdateSession(ctx, childSess); err != nil {
			slog.Warn("Failed to persist child session after delegation", "session_id", childSess.ID, "error", err)
		}
	}

	d.SetLastReply(reply)
	d.StoreStatus(StatusCompleted)
	close(d.DoneCh)

	return d.ID, reply, nil
}

// Continue sends a follow-up message to an existing delegation session.
func (m *Manager) Continue(ctx context.Context, shortID string, message string) (string, error) {
	m.mu.RLock()
	d, ok := m.delegations[shortID]
	m.mu.RUnlock()

	if !ok {
		return "", fmt.Errorf("delegation not found: %s", shortID)
	}

	if message == "" {
		return "", fmt.Errorf("message is required")
	}

	// Load the existing child session from the store so we preserve
	// full conversation history from previous turns.
	var childSess *session.Session
	if m.sessionStore != nil {
		if existing, err := m.sessionStore.GetSession(ctx, d.SessionID); err == nil && existing != nil {
			childSess = existing
			childSess.AddMessage(session.UserMessage(message))
		} else {
			slog.Warn("Failed to load existing child session for continuation, creating new", "session_id", d.SessionID, "error", err)
		}
	}

	// Fallback: create a new session if loading from store failed
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

	// Recreate DoneCh so callers waiting on this continuation get a clean signal.
	d.DoneCh = make(chan struct{})
	d.StoreStatus(StatusRunning)

	slog.Debug("Continuing delegation", "delegation_id", shortID, "session_id", d.SessionID, "agent", d.AgentName)

	reply, err := m.runner.RunDelegation(ctx, d, childSess)
	if err != nil {
		d.SetError(err)
		d.StoreStatus(StatusFailed)
		close(d.DoneCh)
		return "", err
	}

	if m.sessionStore != nil {
		if err := m.sessionStore.UpdateSession(ctx, childSess); err != nil {
			slog.Warn("Failed to persist child session after continuation", "session_id", childSess.ID, "error", err)
		}
	}

	d.SetLastReply(reply)
	d.StoreStatus(StatusCompleted)
	close(d.DoneCh)
	return reply, nil
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

	slog.Debug("Delegation stopped", "delegation_id", shortID)
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

// StopAll cancels all running delegations.
func (m *Manager) StopAll() {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, d := range m.delegations {
		if d.CompareAndSwapStatus(StatusRunning, StatusCancelled) {
			if d.Cancel != nil {
				d.Cancel()
			}
		}
	}
}
