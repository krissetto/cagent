package delegation

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// DelegationStatus represents the lifecycle state of a delegation
type DelegationStatus int32

const (
	StatusPending   DelegationStatus = 0
	StatusRunning   DelegationStatus = 1
	StatusCompleted DelegationStatus = 2
	StatusFailed    DelegationStatus = 3
	StatusCancelled DelegationStatus = 4
)

var statusToString = map[DelegationStatus]string{
	StatusPending:   "pending",
	StatusRunning:   "running",
	StatusCompleted: "completed",
	StatusFailed:    "failed",
	StatusCancelled: "cancelled",
}

// String returns the string representation of a delegation status
func (s DelegationStatus) String() string {
	if str, ok := statusToString[s]; ok {
		return str
	}
	return "unknown"
}

// Delegation represents a single delegation from parent to child session.
// The delegation_id IS the child session ID.
type Delegation struct {
	// SessionID is the unique identifier for this delegation (== child session ID)
	SessionID string

	// ParentSessionID is the parent session ID
	ParentSessionID string

	// AgentName is the name of the agent being delegated to
	AgentName string

	// Status is the current lifecycle status (atomic)
	Status atomic.Int32

	// Cancel is the context cancellation function for the delegation
	Cancel context.CancelFunc

	// DoneCh is closed when the delegation completes (for sync await)
	DoneCh chan struct{}

	// lastReply stores the latest child assistant message
	mu        sync.Mutex
	lastReply string

	// err stores any error from delegation execution
	err error

	// StartTime is when this delegation started
	StartTime time.Time
}

// NewDelegation creates a new delegation for a child session
func NewDelegation(sessionID, parentSessionID, agentName string) *Delegation {
	return &Delegation{
		SessionID:       sessionID,
		ParentSessionID: parentSessionID,
		AgentName:       agentName,
		DoneCh:          make(chan struct{}),
		StartTime:       time.Now(),
	}
}

// LoadStatus returns the current status atomically
func (d *Delegation) LoadStatus() DelegationStatus {
	return DelegationStatus(d.Status.Load())
}

// StoreStatus sets the status atomically
func (d *Delegation) StoreStatus(s DelegationStatus) {
	d.Status.Store(int32(s))
}

// CompareAndSwapStatus atomically updates status if it matches old
func (d *Delegation) CompareAndSwapStatus(old, next DelegationStatus) bool {
	return d.Status.CompareAndSwap(int32(old), int32(next))
}

// SetLastReply updates the latest child assistant message
func (d *Delegation) SetLastReply(reply string) {
	d.mu.Lock()
	d.lastReply = reply
	d.mu.Unlock()
}

// GetLastReply returns the latest child assistant message
func (d *Delegation) GetLastReply() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lastReply
}

// SetError sets the error for a failed delegation
func (d *Delegation) SetError(err error) {
	d.mu.Lock()
	d.err = err
	d.mu.Unlock()
}

// GetError returns any error from delegation execution
func (d *Delegation) GetError() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.err
}

// Duration returns the elapsed time since the delegation started
func (d *Delegation) Duration() time.Duration {
	return time.Since(d.StartTime)
}
