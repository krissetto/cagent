package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

const (
	ToolNameSubagentStart    = "subagent_start"
	ToolNameSubagentSend     = "subagent_send"
	ToolNameSubagentInspect  = "subagent_inspect"
	ToolNameSubagentList     = "subagent_list"
	ToolNameSubagentStop     = "subagent_stop"
	ToolNameSubagentFinalize = "subagent_finalize"
)

const (
	managedSubagentStatusRunning    = "running"
	managedSubagentStatusCompleted  = "completed"
	managedSubagentStatusStopped    = "stopped"
	managedSubagentStatusFinalizing = "finalizing"
	managedSubagentStatusFailed     = "failed"
)

type SubagentStartArgs struct {
	Agent string `json:"agent" jsonschema:"The name of the runtime-managed subagent to start."`
	Task  string `json:"task" jsonschema:"A clear and concise description of the task the subagent should begin with."`
}

type SubagentSendArgs struct {
	SubagentID string `json:"subagent_id" jsonschema:"The child session ID, or a unique prefix of it."`
	Message    string `json:"message" jsonschema:"The message to send to the runtime-managed subagent."`
	Mode       string `json:"mode,omitempty" jsonschema:"How to deliver the message: followup (default) or steer."`
}

type SubagentInspectArgs struct {
	SubagentID string `json:"subagent_id" jsonschema:"The child session ID, or a unique prefix of it."`
	Mode       string `json:"mode,omitempty" jsonschema:"How much transcript context to return: last (default), recent, or full."`
}

type SubagentStopArgs struct {
	SubagentID string `json:"subagent_id" jsonschema:"The child session ID, or a unique prefix of it."`
}

type managedSubagent struct {
	id          string
	parentID    string
	rootID      string
	agentName   string
	task        string
	session     *session.Session
	startTime   time.Time
	completedAt time.Time

	cancel context.CancelFunc
	queue  *managedSubagentQueue

	mu     sync.RWMutex
	status string
	errMsg string
	result string
	events []Event
}

type managedSubagentQueue struct {
	steer    MessageQueue
	followUp MessageQueue
}

type managedQueuesContextKey struct{}

type managedSubagentManager struct {
	runtime *LocalRuntime
	mu      sync.RWMutex
	items   map[string]*managedSubagent
}

type SubagentLifecycleEvent struct {
	AgentContext

	Type       string `json:"type"`
	SessionID  string `json:"session_id"`
	ParentID   string `json:"parent_id"`
	RootID     string `json:"root_id"`
	Agent      string `json:"agent"`
	Status     string `json:"status"`
	Result     string `json:"result,omitempty"`
	Error      string `json:"error,omitempty"`
	OccurredAt string `json:"occurred_at"`
}

func (e *SubagentLifecycleEvent) GetSessionID() string { return e.SessionID }

func newManagedSubagentManager(r *LocalRuntime) *managedSubagentManager {
	return &managedSubagentManager{
		runtime: r,
		items:   make(map[string]*managedSubagent),
	}
}

func (m *managedSubagentManager) registerDefaultTools() {
	m.runtime.toolMap[ToolNameSubagentStart] = m.handleStart
	m.runtime.toolMap[ToolNameSubagentSend] = m.handleSend
	m.runtime.toolMap[ToolNameSubagentInspect] = m.handleInspect
	m.runtime.toolMap[ToolNameSubagentList] = m.handleList
	m.runtime.toolMap[ToolNameSubagentStop] = m.handleStop
	m.runtime.toolMap[ToolNameSubagentFinalize] = m.handleFinalize
}

func (m *managedSubagentManager) handleStart(ctx context.Context, parent *session.Session, toolCall tools.ToolCall, events EventSink) (*tools.ToolCallResult, error) {
	var args SubagentStartArgs
	if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	args.Agent = strings.TrimSpace(args.Agent)
	args.Task = strings.TrimSpace(args.Task)
	if args.Agent == "" {
		return tools.ResultError("agent is required"), nil
	}
	if args.Task == "" {
		return tools.ResultError("task is required"), nil
	}

	current := m.runtime.resolveSessionAgent(parent)
	if validation := validateAgentInList(current.Name(), args.Agent, "start runtime-managed subagent", "sub-agents list", current.SubAgents()); validation != nil {
		return validation, nil
	}
	childAgent, err := m.runtime.team.Agent(args.Agent)
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}

	child := newSubSession(parent, SubSessionConfig{
		Task:           args.Task,
		AgentName:      childAgent.Name(),
		PinAgent:       true,
		NonInteractive: true,
	}, childAgent)
	if m.runtime.sessionStore != nil {
		if err := m.runtime.sessionStore.AddSubSession(ctx, parent.ID, child); err != nil {
			if !errors.Is(err, session.ErrNotFound) {
				return nil, err
			}
			// Some tests and embedders invoke the tool handler with an in-memory
			// parent that has not been persisted yet. Persist the parent and then
			// link the managed child so production paths still use AddSubSession.
			if addErr := m.runtime.sessionStore.AddSession(ctx, parent); addErr != nil {
				return nil, addErr
			}
			if addErr := m.runtime.sessionStore.AddSubSession(ctx, parent.ID, child); addErr != nil {
				return nil, addErr
			}
		}
	}

	childCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	ms := &managedSubagent{
		id:        child.ID,
		parentID:  parent.ID,
		rootID:    child.EffectiveRootID(),
		agentName: childAgent.Name(),
		task:      args.Task,
		session:   child,
		startTime: m.runtime.now(),
		cancel:    cancel,
		queue: &managedSubagentQueue{
			steer:    NewInMemoryMessageQueue(defaultSteerQueueCapacity),
			followUp: NewInMemoryMessageQueue(defaultFollowUpQueueCapacity),
		},
		status: managedSubagentStatusRunning,
	}

	m.mu.Lock()
	m.items[child.ID] = ms
	m.mu.Unlock()

	m.emitLifecycle(ctx, ms, managedSubagentStatusRunning, "", events)
	go m.run(childCtx, ms)

	return tools.ResultSuccess(fmt.Sprintf("Runtime-managed subagent started.\nID: %s\nAgent: %s\nStatus: %s", child.ID, childAgent.Name(), managedSubagentStatusRunning)), nil
}

func (m *managedSubagentManager) run(ctx context.Context, ms *managedSubagent) {
	ctx = context.WithValue(ctx, managedQueuesContextKey{}, ms.queue)
	childEvents := m.runtime.RunStream(ctx, ms.session)
	var errMsg string
	for event := range childEvents {
		ms.appendEvent(event)
		if errorEvent, ok := event.(*ErrorEvent); ok {
			errMsg = strings.TrimSpace(errorEvent.Error)
		}
	}

	status := managedSubagentStatusCompleted
	if errMsg != "" {
		status = managedSubagentStatusFailed
	}
	if ctx.Err() != nil {
		if ms.loadStatus() == managedSubagentStatusFinalizing {
			status = managedSubagentStatusCompleted
		} else {
			status = managedSubagentStatusStopped
		}
	}
	result := lastAssistantContent(ms.session)
	ms.finish(status, result, errMsg, m.runtime.now())

	if m.runtime.sessionStore != nil {
		_ = m.runtime.sessionStore.UpdateSession(context.WithoutCancel(ctx), ms.session)
	}
	m.emitLifecycle(context.WithoutCancel(ctx), ms, status, result, nil)
}

func (m *managedSubagentManager) handleSend(ctx context.Context, sess *session.Session, toolCall tools.ToolCall, _ EventSink) (*tools.ToolCallResult, error) {
	var args SubagentSendArgs
	if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	ms, err := m.resolveForSession(strings.TrimSpace(args.SubagentID), sess)
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}
	if !ms.isRunning() {
		return tools.ResultError(fmt.Sprintf("subagent %s is not running (status: %s)", ms.id, ms.loadStatus())), nil
	}
	args.Message = strings.TrimSpace(args.Message)
	if args.Message == "" {
		return tools.ResultError("message is required"), nil
	}
	msg := QueuedMessage{Content: args.Message}
	switch strings.ToLower(strings.TrimSpace(args.Mode)) {
	case "", "followup", "follow-up":
		if ok := ms.queue.followUp.Enqueue(ctx, msg); !ok {
			return tools.ResultError("follow-up queue is full"), nil
		}
		return tools.ResultSuccess(fmt.Sprintf("Follow-up queued for subagent %s.", ms.id)), nil
	case "steer":
		if ok := ms.queue.steer.Enqueue(ctx, msg); !ok {
			return tools.ResultError("steer queue is full"), nil
		}
		return tools.ResultSuccess(fmt.Sprintf("Steer queued for subagent %s.", ms.id)), nil
	default:
		return tools.ResultError("mode must be followup or steer"), nil
	}
}

func (m *managedSubagentManager) handleInspect(_ context.Context, sess *session.Session, toolCall tools.ToolCall, _ EventSink) (*tools.ToolCallResult, error) {
	var args SubagentInspectArgs
	if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	ms, err := m.resolveForSession(strings.TrimSpace(args.SubagentID), sess)
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}
	mode := strings.ToLower(strings.TrimSpace(args.Mode))
	if mode == "" {
		mode = "last"
	}
	messages := ms.session.GetAllMessages()
	switch mode {
	case "last":
		if len(messages) > 1 {
			messages = messages[len(messages)-1:]
		}
	case "recent":
		if len(messages) > 6 {
			messages = messages[len(messages)-6:]
		}
	case "full":
		// keep all messages
	default:
		return tools.ResultError("mode must be last, recent, or full"), nil
	}

	var out strings.Builder
	fmt.Fprintf(&out, "ID: %s\nAgent: %s\nStatus: %s\n", ms.id, ms.agentName, ms.loadStatus())
	if result := ms.loadResult(); result != "" {
		fmt.Fprintf(&out, "Result: %s\n", result)
	}
	if errMsg := ms.loadError(); errMsg != "" {
		fmt.Fprintf(&out, "Error: %s\n", errMsg)
	}
	out.WriteString("Transcript:\n")
	for _, msg := range messages {
		fmt.Fprintf(&out, "- %s", msg.Message.Role)
		if msg.AgentName != "" {
			fmt.Fprintf(&out, " (%s)", msg.AgentName)
		}
		fmt.Fprintf(&out, ": %s\n", msg.Message.Content)
	}
	return tools.ResultSuccess(out.String()), nil
}

func (m *managedSubagentManager) handleList(_ context.Context, parent *session.Session, _ tools.ToolCall, _ EventSink) (*tools.ToolCallResult, error) {
	rootID := parent.EffectiveRootID()
	var list []*managedSubagent
	m.mu.RLock()
	for _, ms := range m.items {
		if ms.rootID == rootID || ms.parentID == parent.ID {
			list = append(list, ms)
		}
	}
	m.mu.RUnlock()
	slices.SortFunc(list, func(a, b *managedSubagent) int { return a.startTime.Compare(b.startTime) })

	if len(list) == 0 {
		return tools.ResultSuccess("No runtime-managed subagents found.\n"), nil
	}
	var out strings.Builder
	out.WriteString("Runtime-managed subagents:\n\n")
	for _, ms := range list {
		fmt.Fprintf(&out, "ID: %s\n", ms.id)
		fmt.Fprintf(&out, "  Parent:  %s\n", ms.parentID)
		fmt.Fprintf(&out, "  Agent:   %s\n", ms.agentName)
		fmt.Fprintf(&out, "  Status:  %s\n", ms.loadStatus())
		fmt.Fprintf(&out, "  Runtime: %s\n", m.runtime.now().Sub(ms.startTime).Round(time.Second))
		if errMsg := ms.loadError(); errMsg != "" {
			fmt.Fprintf(&out, "  Error:   %s\n", errMsg)
		}
		out.WriteString("\n")
	}
	return tools.ResultSuccess(out.String()), nil
}

func (m *managedSubagentManager) handleStop(ctx context.Context, sess *session.Session, toolCall tools.ToolCall, _ EventSink) (*tools.ToolCallResult, error) {
	var args SubagentStopArgs
	if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	ms, err := m.resolveForSession(strings.TrimSpace(args.SubagentID), sess)
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}
	if !ms.isRunning() {
		return tools.ResultError(fmt.Sprintf("subagent %s is not running (status: %s)", ms.id, ms.loadStatus())), nil
	}
	ms.storeStatus(managedSubagentStatusStopped, m.runtime.now())
	m.emitLifecycle(ctx, ms, managedSubagentStatusStopped, "", nil)
	ms.cancel()
	return tools.ResultSuccess(fmt.Sprintf("Subagent %s stopped.", ms.id)), nil
}

func (m *managedSubagentManager) handleFinalize(ctx context.Context, sess *session.Session, toolCall tools.ToolCall, _ EventSink) (*tools.ToolCallResult, error) {
	var args SubagentStopArgs
	if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	ms, err := m.resolveForSession(strings.TrimSpace(args.SubagentID), sess)
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}
	if !ms.isRunning() {
		return tools.ResultError(fmt.Sprintf("subagent %s is not running (status: %s)", ms.id, ms.loadStatus())), nil
	}
	ms.storeStatus(managedSubagentStatusFinalizing, m.runtime.now())
	m.emitLifecycle(ctx, ms, managedSubagentStatusFinalizing, "", nil)
	if ok := ms.queue.followUp.Enqueue(ctx, QueuedMessage{Content: "Please finalize cleanly after your current safe point."}); !ok {
		return tools.ResultError("follow-up queue is full"), nil
	}
	return tools.ResultSuccess(fmt.Sprintf("Subagent %s finalization requested.", ms.id)), nil
}

func (m *managedSubagentManager) resolveForSession(id string, caller *session.Session) (*managedSubagent, error) {
	ms, err := m.resolve(id)
	if err != nil {
		return nil, err
	}
	if caller == nil {
		return nil, errors.New("caller session is required")
	}
	callerRoot := caller.EffectiveRootID()
	if ms.rootID != callerRoot && ms.parentID != caller.ID {
		return nil, fmt.Errorf("subagent not found: %s", id)
	}
	return ms, nil
}

func (m *managedSubagentManager) resolve(id string) (*managedSubagent, error) {
	if id == "" {
		return nil, errors.New("subagent_id is required")
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if ms, ok := m.items[id]; ok {
		return ms, nil
	}
	var match *managedSubagent
	for key, ms := range m.items {
		if strings.HasPrefix(key, id) {
			if match != nil {
				return nil, fmt.Errorf("subagent_id %q is ambiguous", id)
			}
			match = ms
		}
	}
	if match == nil {
		return nil, fmt.Errorf("subagent not found: %s", id)
	}
	return match, nil
}

func (m *managedSubagentManager) stopAll() {
	m.mu.RLock()
	list := make([]*managedSubagent, 0, len(m.items))
	for _, ms := range m.items {
		list = append(list, ms)
	}
	m.mu.RUnlock()
	for _, ms := range list {
		if ms.isRunning() {
			ms.storeStatus(managedSubagentStatusStopped, m.runtime.now())
			ms.cancel()
		}
	}
}

func (m *managedSubagentManager) emitLifecycle(ctx context.Context, ms *managedSubagent, status, result string, liveSink EventSink) {
	event := &SubagentLifecycleEvent{
		Type:         "subagent_lifecycle",
		SessionID:    ms.id,
		ParentID:     ms.parentID,
		RootID:       ms.rootID,
		Agent:        ms.agentName,
		Status:       status,
		Result:       result,
		Error:        ms.loadError(),
		OccurredAt:   m.runtime.now().Format(time.RFC3339Nano),
		AgentContext: newAgentContext(ms.agentName),
	}
	if liveSink != nil {
		liveSink.Emit(event)
	}
	if m.runtime.sessionStore == nil {
		return
	}
	for _, obs := range m.runtime.observers {
		obs.OnEvent(ctx, ms.session, event)
	}
}

func (ms *managedSubagent) appendEvent(event Event) {
	ms.mu.Lock()
	ms.events = append(ms.events, event)
	ms.mu.Unlock()
}

func (ms *managedSubagent) loadStatus() string {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	return ms.status
}

func (ms *managedSubagent) storeStatus(status string, completedAt time.Time) {
	ms.mu.Lock()
	ms.status = status
	if status != managedSubagentStatusRunning && status != managedSubagentStatusFinalizing && ms.completedAt.IsZero() {
		ms.completedAt = completedAt
	}
	ms.mu.Unlock()
}

func (ms *managedSubagent) isRunning() bool {
	status := ms.loadStatus()
	return status == managedSubagentStatusRunning || status == managedSubagentStatusFinalizing
}

func (ms *managedSubagent) finish(status, result, errMsg string, completedAt time.Time) {
	ms.mu.Lock()
	if ms.status == managedSubagentStatusStopped && status != managedSubagentStatusFailed {
		status = managedSubagentStatusStopped
	}
	ms.status = status
	ms.result = result
	ms.errMsg = errMsg
	ms.completedAt = completedAt
	ms.mu.Unlock()
}

func (ms *managedSubagent) loadResult() string {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	return ms.result
}

func (ms *managedSubagent) loadError() string {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	return ms.errMsg
}

func lastAssistantContent(sess *session.Session) string {
	for _, msg := range slices.Backward(sess.GetAllMessages()) {
		if msg.Message.Role == chat.MessageRoleAssistant {
			return msg.Message.Content
		}
	}
	return ""
}
