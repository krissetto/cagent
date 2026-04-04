package delegation

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker-agent/pkg/concurrent"
	"github.com/docker/docker-agent/pkg/session"
)

const (
	// DefaultMaxConcurrent is the maximum number of simultaneously running delegations
	DefaultMaxConcurrent = 20
	// DefaultMaxTotal caps total stored delegations (running + completed)
	DefaultMaxTotal = 100
	// DefaultMaxDepth caps delegation nesting depth
	DefaultMaxDepth = 5
	// DefaultMaxOutputBytes caps output buffer per delegation
	DefaultMaxOutputBytes = 10 * 1024 * 1024 // 10 MB
)

// DelegationRunner abstracts the runtime dependency for actually running a sub-session.
type DelegationRunner interface {
	// RunDelegation starts a sub-agent and blocks until completion or cancellation.
	// It is the replacement for both runSubSessionCollecting and runSubSessionForwarding.
	RunDelegation(ctx context.Context, params RunParams) *RunResult
}

// RunParams holds the parameters for running a delegation.
type RunParams struct {
	AgentName      string
	Task           string
	ExpectedOutput string
	ParentSession  *session.Session
	OnContent      func(content string)
	// SyncEvents, when non-nil, indicates sync mode: events are forwarded to this channel
	SyncEvents chan<- any
}

// RunResult holds the outcome of a delegation execution.
type RunResult struct {
	Result       string           // final assistant message on completion
	ErrMsg       string           // error detail if failed
	Stopped      bool             // true if delegation was cancelled
	ChildSession *session.Session // sub-session created by the delegation (for SubSessionCompletedEvent)
}

// CompletionCallback is invoked when an async delegation completes.
// The runtime uses this to inject DelegationCompletedEvent into the parent's event channel.
type CompletionCallback func(d *Delegation)

// DelegationNode represents a node in the delegation tree for display purposes.
type DelegationNode struct {
	ID        string            `json:"id"`
	AgentName string            `json:"agent_name"`
	Task      string            `json:"task"`
	Status    DelegationStatus  `json:"status"`
	Mode      DelegationMode    `json:"mode"`
	StartTime time.Time         `json:"start_time"`
	Duration  time.Duration     `json:"duration"`
	Children  []*DelegationNode `json:"children,omitempty"`
}

// contextKey is a private type for context keys in this package.
type contextKey string

// ContextKeyDelegationID is the context key used to thread the current delegation
// ID through sub-agent execution. Handlers read this to populate parentDelegationID
// when creating nested delegations.
const ContextKeyDelegationID contextKey = "delegation_id"

// Manager owns all delegations and provides lifecycle management.
// It replaces agenttool.Handler and consolidates sync/async/handoff delegation paths.
type Manager struct {
	mu          sync.Mutex
	delegations *concurrent.Map[string, *Delegation]
	wg          sync.WaitGroup
	stopped     bool // prevents new admissions after StopAll

	maxConcurrent  int
	maxTotal       int
	maxDepth       int
	maxOutputBytes int

	runner     DelegationRunner
	onComplete CompletionCallback
}

// ManagerOption configures the Manager.
type ManagerOption func(*Manager)

// WithMaxConcurrent sets the maximum number of concurrent delegations.
func WithMaxConcurrent(n int) ManagerOption {
	return func(m *Manager) { m.maxConcurrent = n }
}

// WithMaxTotal sets the maximum total number of delegations.
func WithMaxTotal(n int) ManagerOption {
	return func(m *Manager) { m.maxTotal = n }
}

// WithMaxDepth sets the maximum delegation nesting depth.
func WithMaxDepth(n int) ManagerOption {
	return func(m *Manager) { m.maxDepth = n }
}

// WithMaxOutputBytes sets the maximum output buffer size per delegation.
func WithMaxOutputBytes(n int) ManagerOption {
	return func(m *Manager) { m.maxOutputBytes = n }
}

// WithCompletionCallback sets the callback invoked when async delegations complete.
func WithCompletionCallback(cb CompletionCallback) ManagerOption {
	return func(m *Manager) { m.onComplete = cb }
}

// NewManager creates a new DelegationManager with the given runner and options.
func NewManager(runner DelegationRunner, opts ...ManagerOption) *Manager {
	m := &Manager{
		delegations:    concurrent.NewMap[string, *Delegation](),
		maxConcurrent:  DefaultMaxConcurrent,
		maxTotal:       DefaultMaxTotal,
		maxDepth:       DefaultMaxDepth,
		maxOutputBytes: DefaultMaxOutputBytes,
		runner:         runner,
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Prepare creates and registers a delegation without starting it.
// The caller should emit events (e.g. DelegationStarted) between Prepare and Launch.
func (m *Manager) Prepare(parentSession *session.Session, parentDelegationID, agentName, task, expectedOutput string, mode DelegationMode) (*Delegation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.stopped {
		return nil, fmt.Errorf("delegation manager is stopped; no new delegations accepted")
	}

	// Check nesting depth
	if parentDelegationID != "" {
		depth := m.depthOf(parentDelegationID)
		if depth >= m.maxDepth {
			return nil, fmt.Errorf("maximum delegation depth (%d) exceeded", m.maxDepth)
		}
	}

	// Enforce concurrency cap for async delegations
	if mode == ModeAsyncDelegate {
		if m.runningCount() >= m.maxConcurrent {
			return nil, fmt.Errorf("maximum concurrent delegations (%d) reached; stop or wait for existing delegations", m.maxConcurrent)
		}
	}

	// Enforce total cap
	if m.delegations.Length() >= m.maxTotal {
		m.pruneCompleted()
		if m.delegations.Length() >= m.maxTotal {
			return nil, fmt.Errorf("maximum total delegations (%d) reached", m.maxTotal)
		}
	}

	d := NewDelegation(parentSession.ID, parentDelegationID, agentName, task, expectedOutput, mode)
	d.ParentSession = parentSession
	d.MaxOutput = m.maxOutputBytes
	d.StoreStatus(StatusRunning)

	m.delegations.Store(d.ID, d)

	// Link to parent
	if parentDelegationID != "" {
		if parent, ok := m.delegations.Load(parentDelegationID); ok {
			parent.AddChild(d.ID)
		}
	}

	return d, nil
}

// Launch starts a previously prepared delegation. For sync mode, it blocks until
// completion. For async mode, it returns immediately.
func (m *Manager) Launch(ctx context.Context, d *Delegation) error {
	switch d.Mode {
	case ModeAsyncDelegate:
		m.runAsync(ctx, d)
		return nil
	case ModeSyncDelegate:
		m.runSync(ctx, d)
		return nil
	case ModeHandoff:
		d.StoreStatus(StatusCompleted)
		close(d.DoneCh)
		return nil
	default:
		return fmt.Errorf("unsupported delegation mode: %s", d.Mode)
	}
}

// Start creates and launches a new delegation. For async mode, it returns immediately
// with the delegation ID. For sync mode, it blocks until completion.
func (m *Manager) Start(ctx context.Context, parentSession *session.Session, parentDelegationID, agentName, task, expectedOutput string, mode DelegationMode) (*Delegation, error) {
	d, err := m.Prepare(parentSession, parentDelegationID, agentName, task, expectedOutput, mode)
	if err != nil {
		return nil, err
	}
	if err := m.Launch(ctx, d); err != nil {
		return nil, err
	}
	return d, nil
}

// runAsync launches a delegation in a goroutine
func (m *Manager) runAsync(ctx context.Context, d *Delegation) {
	// Use context.WithoutCancel so parent cancel doesn't kill async children
	// (lesson from background-agent-fixes)
	childCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	// Inject delegation ID so nested handlers can populate parentDelegationID
	childCtx = context.WithValue(childCtx, ContextKeyDelegationID, d.ID)
	d.Cancel = cancel

	m.wg.Go(func() {
		defer cancel()
		defer close(d.DoneCh)

		slog.Debug("Starting async delegation", "delegation_id", d.ID, "agent", d.AgentName)

		result := m.runner.RunDelegation(childCtx, RunParams{
			AgentName:      d.AgentName,
			Task:           d.Task,
			ExpectedOutput: d.ExpectedOutput,
			ParentSession:  d.ParentSession,
			OnContent: func(content string) {
				d.AppendOutput(content)
			},
		})

		m.finishDelegation(d, result)

		// Fire completion callback for event-driven parent resume
		if m.onComplete != nil {
			m.onComplete(d)
		}
	})
}

// runSync runs a delegation synchronously, blocking until completion
func (m *Manager) runSync(ctx context.Context, d *Delegation) {
	// Inject delegation ID so nested handlers can populate parentDelegationID
	childCtx, cancel := context.WithCancel(context.WithValue(ctx, ContextKeyDelegationID, d.ID))
	d.Cancel = cancel

	defer cancel()
	defer close(d.DoneCh)

	slog.Debug("Starting sync delegation", "delegation_id", d.ID, "agent", d.AgentName)

	result := m.runner.RunDelegation(childCtx, RunParams{
		AgentName:      d.AgentName,
		Task:           d.Task,
		ExpectedOutput: d.ExpectedOutput,
		ParentSession:  d.ParentSession,
		OnContent: func(content string) {
			d.AppendOutput(content)
		},
	})

	m.finishDelegation(d, result)
}

// finishDelegation sets the final status and result for a delegation
func (m *Manager) finishDelegation(d *Delegation, result *RunResult) {
	if result == nil {
		d.ErrMsg = "nil result"
		d.StoreStatus(StatusFailed)
		return
	}

	if result.Stopped {
		d.CompareAndSwapStatus(StatusRunning, StatusStopped)
		slog.Debug("Delegation stopped", "delegation_id", d.ID)
		return
	}

	if result.ErrMsg != "" {
		d.ErrMsg = result.ErrMsg
		d.StoreStatus(StatusFailed)
		slog.Debug("Delegation failed", "delegation_id", d.ID, "error", result.ErrMsg)
		return
	}

	d.Result = result.Result
	d.ChildSession = result.ChildSession
	if d.CompareAndSwapStatus(StatusRunning, StatusCompleted) {
		slog.Debug("Delegation completed", "delegation_id", d.ID, "agent", d.AgentName)
	}
}

// Get returns a delegation by ID
func (m *Manager) Get(id string) (*Delegation, bool) {
	return m.delegations.Load(id)
}

// List returns a summary of all delegations
func (m *Manager) List() string {
	var out strings.Builder
	out.WriteString("Delegations:\n\n")

	var count int
	m.delegations.Range(func(_ string, d *Delegation) bool {
		count++
		status := d.LoadStatus()
		elapsed := d.Duration().Round(time.Second)
		fmt.Fprintf(&out, "ID: %s\n", d.ID)
		fmt.Fprintf(&out, "  Agent:    %s\n", d.AgentName)
		fmt.Fprintf(&out, "  Mode:     %s\n", d.Mode)
		fmt.Fprintf(&out, "  Status:   %s\n", status)
		fmt.Fprintf(&out, "  Runtime:  %s\n", elapsed)
		out.WriteString("\n")
		return true
	})

	if count == 0 {
		out.WriteString("No delegations found.\n")
	}

	return out.String()
}

// View returns status and output for a specific delegation
func (m *Manager) View(id string) (string, error) {
	d, ok := m.delegations.Load(id)
	if !ok {
		return "", fmt.Errorf("delegation not found: %s", id)
	}

	status := d.LoadStatus()
	elapsed := d.Duration().Round(time.Second)

	var out strings.Builder
	fmt.Fprintf(&out, "Delegation ID: %s\n", d.ID)
	fmt.Fprintf(&out, "Agent:         %s\n", d.AgentName)
	fmt.Fprintf(&out, "Mode:          %s\n", d.Mode)
	fmt.Fprintf(&out, "Status:        %s\n", status)
	fmt.Fprintf(&out, "Runtime:       %s\n", elapsed)
	out.WriteString("\n--- Output ---\n")

	switch status {
	case StatusCompleted:
		if d.Result != "" {
			out.WriteString(d.Result)
		} else {
			out.WriteString("<no output>")
		}
	case StatusFailed:
		out.WriteString("<delegation failed>")
		if d.ErrMsg != "" {
			fmt.Fprintf(&out, "\nError: %s", d.ErrMsg)
		}
	case StatusStopped:
		out.WriteString("<delegation was stopped>")
	default:
		output := d.GetOutput()
		if output != "" {
			out.WriteString(output)
			if d.MaxOutput > 0 && len(output) >= d.MaxOutput {
				out.WriteString("\n\n[output truncated at limit — still running...]")
			} else {
				out.WriteString("\n\n[still running...]")
			}
		} else {
			out.WriteString("<no output yet — still running>")
		}
	}

	return out.String(), nil
}

// Stop cancels a running delegation
func (m *Manager) Stop(id string) error {
	d, ok := m.delegations.Load(id)
	if !ok {
		return fmt.Errorf("delegation not found: %s", id)
	}

	if !d.CompareAndSwapStatus(StatusRunning, StatusStopped) {
		current := d.LoadStatus()
		return fmt.Errorf("delegation %s is not running (status: %s)", id, current)
	}

	if d.Cancel != nil {
		d.Cancel()
	}

	return nil
}

// StopAll cancels all running delegations and waits for goroutines to exit.
func (m *Manager) StopAll() {
	m.mu.Lock()
	m.stopped = true
	m.mu.Unlock()

	m.delegations.Range(func(_ string, d *Delegation) bool {
		if d.CompareAndSwapStatus(StatusRunning, StatusStopped) {
			if d.Cancel != nil {
				d.Cancel()
			}
		}
		return true
	})
	m.wg.Wait()
}

// Tree builds a hierarchical tree view of all delegations
func (m *Manager) Tree() []*DelegationNode {
	// Build map of all delegations
	allDelegations := make(map[string]*Delegation)
	m.delegations.Range(func(id string, d *Delegation) bool {
		allDelegations[id] = d
		return true
	})

	// Find root delegations (no parent delegation)
	var roots []*DelegationNode
	for _, d := range allDelegations {
		if d.ParentDelegationID == "" {
			roots = append(roots, m.buildNode(d, allDelegations))
		}
	}
	return roots
}

func (m *Manager) buildNode(d *Delegation, allDelegations map[string]*Delegation) *DelegationNode {
	node := &DelegationNode{
		ID:        d.ID,
		AgentName: d.AgentName,
		Task:      d.Task,
		Status:    d.LoadStatus(),
		Mode:      d.Mode,
		StartTime: d.StartTime,
		Duration:  d.Duration(),
	}

	for _, childID := range d.GetChildren() {
		if child, ok := allDelegations[childID]; ok {
			node.Children = append(node.Children, m.buildNode(child, allDelegations))
		}
	}
	return node
}

// runningCount returns the number of currently running delegations (caller must NOT hold mu)
func (m *Manager) runningCount() int {
	var count int
	m.delegations.Range(func(_ string, d *Delegation) bool {
		if d.LoadStatus() == StatusRunning {
			count++
		}
		return true
	})
	return count
}

// depthOf calculates the nesting depth of a delegation
func (m *Manager) depthOf(delegationID string) int {
	depth := 0
	current := delegationID
	for current != "" {
		depth++
		d, ok := m.delegations.Load(current)
		if !ok {
			break
		}
		current = d.ParentDelegationID
	}
	return depth
}

// pruneCompleted removes completed/stopped/failed delegations
func (m *Manager) pruneCompleted() {
	var toDelete []string
	m.delegations.Range(func(id string, d *Delegation) bool {
		s := d.LoadStatus()
		if s == StatusCompleted || s == StatusStopped || s == StatusFailed {
			toDelete = append(toDelete, id)
		}
		return true
	})
	for _, id := range toDelete {
		m.delegations.Delete(id)
	}
}
