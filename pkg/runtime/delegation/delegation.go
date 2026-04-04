package delegation

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/docker/docker-agent/pkg/session"
)

// DelegationMode specifies how a delegation should execute
type DelegationMode string

const (
	// ModeSyncDelegate: Parent blocks until child completes, result returned inline
	ModeSyncDelegate DelegationMode = "sync"
	// ModeAsyncDelegate: Child runs in background, parent continues, results via event
	ModeAsyncDelegate DelegationMode = "async"
	// ModeHandoff: Legacy handoff behavior - switches currentAgent, doesn't create child session
	ModeHandoff DelegationMode = "handoff"
)

// DelegationStatus represents the lifecycle state of a delegation
type DelegationStatus int32

const (
	StatusPending DelegationStatus = iota
	StatusRunning
	StatusCompleted
	StatusStopped
	StatusFailed
)

var statusToString = map[DelegationStatus]string{
	StatusPending:   "pending",
	StatusRunning:   "running",
	StatusCompleted: "completed",
	StatusStopped:   "stopped",
	StatusFailed:    "failed",
}

// StatusString returns the string representation of a delegation status
func (s DelegationStatus) String() string {
	if str, ok := statusToString[s]; ok {
		return str
	}
	return "unknown"
}

// Delegation represents a single delegation from parent to child agent
type Delegation struct {
	// ID is the unique identifier for this delegation
	ID string

	// ParentDelegationID is the ID of the parent delegation (empty for root)
	ParentDelegationID string

	// SessionID is the child session ID created for this delegation
	SessionID string

	// ParentSessionID is the parent session ID
	ParentSessionID string

	// ParentSession holds a reference to the parent session (used for sub-session addition)
	ParentSession *session.Session

	// ChildSession is the sub-session created for this delegation, populated after RunDelegation
	// completes. Used to emit SubSessionCompletedEvent via the completion handler.
	ChildSession *session.Session

	// AgentName is the name of the agent being delegated to
	AgentName string

	// Task is the user-facing task description
	Task string

	// ExpectedOutput is optional expected output description
	ExpectedOutput string

	// Mode is the delegation mode (async, sync, handoff)
	Mode DelegationMode

	// Status is the current lifecycle status (atomic)
	Status atomic.Int32

	// Result is the final result from the delegation (when completed)
	Result string

	// ErrMsg is the error message (when failed)
	ErrMsg string

	// Output is the live buffered output from the delegation
	OutputMu sync.RWMutex
	Output   string
	MaxOutput int // When set, cap output at this many bytes

	// StartTime is when this delegation started
	StartTime time.Time

	// Cancel is the context cancellation function for the delegation
	Cancel context.CancelFunc

	// DoneCh is closed when the delegation completes (for sync await)
	DoneCh chan struct{}

	// Children is the list of child delegation IDs
	ChildrenMu sync.RWMutex
	Children   []string

	// Events is a callback that delivers events to the parent stream, captured at
	// delegation creation time. This ensures async completion events are delivered to
	// the correct channel even if the runtime's current events channel has changed.
	// The field uses func(any) bool to avoid a circular import with the runtime package.
	// Returns true if the event was sent, false if the channel was full/nil.
	Events func(any) bool
}

// NewDelegation creates a new delegation
func NewDelegation(parentSessionID, parentDelegationID, agentName, task, expectedOutput string, mode DelegationMode) *Delegation {
	return &Delegation{
		ID:                 "deleg_" + uuid.New().String(),
		ParentDelegationID: parentDelegationID,
		ParentSessionID:    parentSessionID,
		AgentName:          agentName,
		Task:               task,
		ExpectedOutput:     expectedOutput,
		Mode:               mode,
		DoneCh:             make(chan struct{}),
		StartTime:          time.Now(),
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

// AppendOutput appends content to the output, respecting the max limit
func (d *Delegation) AppendOutput(content string) {
	d.OutputMu.Lock()
	defer d.OutputMu.Unlock()

	if d.MaxOutput > 0 && len(d.Output)+len(content) > d.MaxOutput {
		// Truncate to fit within limit
		remaining := d.MaxOutput - len(d.Output)
		if remaining > 0 {
			d.Output += content[:remaining]
		}
	} else {
		d.Output += content
	}
}

// GetOutput returns a copy of the current output
func (d *Delegation) GetOutput() string {
	d.OutputMu.RLock()
	defer d.OutputMu.RUnlock()
	return d.Output
}

// AddChild adds a child delegation ID
func (d *Delegation) AddChild(childID string) {
	d.ChildrenMu.Lock()
	defer d.ChildrenMu.Unlock()
	d.Children = append(d.Children, childID)
}

// GetChildren returns a copy of the children list
func (d *Delegation) GetChildren() []string {
	d.ChildrenMu.RLock()
	defer d.ChildrenMu.RUnlock()
	return append([]string(nil), d.Children...)
}

// Duration returns the elapsed time since the delegation started
func (d *Delegation) Duration() time.Duration {
	return time.Since(d.StartTime)
}
