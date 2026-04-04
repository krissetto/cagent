package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/runtime/delegation"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin"
)

// agentNames returns the names of the given agents.
func agentNames(agents []*agent.Agent) []string {
	names := make([]string, len(agents))
	for i, a := range agents {
		names[i] = a.Name()
	}
	return names
}

// validateAgentInList checks that targetAgent appears in the given agent list.
// Returns a tool error result if not found, or nil if the target is valid.
// The action describes the attempted operation (e.g. "transfer task to"),
// and listDesc is a human-readable description of the list (e.g. "sub-agents list").
func validateAgentInList(currentAgent, targetAgent, action, listDesc string, agents []*agent.Agent) *tools.ToolCallResult {
	if slices.ContainsFunc(agents, func(a *agent.Agent) bool { return a.Name() == targetAgent }) {
		return nil
	}
	if names := agentNames(agents); len(names) > 0 {
		return tools.ResultError(fmt.Sprintf(
			"Agent %s cannot %s %s: target agent not in %s. Available agent IDs are: %s",
			currentAgent, action, targetAgent, listDesc, strings.Join(names, ", "),
		))
	}
	return tools.ResultError(fmt.Sprintf(
		"Agent %s cannot %s %s: target agent not in %s. No agents are configured in this list.",
		currentAgent, action, targetAgent, listDesc,
	))
}

// buildTaskSystemMessage constructs the system message for a delegated task.
func buildTaskSystemMessage(task, expectedOutput string) string {
	msg := "You are a member of a team of agents. Your goal is to complete the following task:"
	msg += fmt.Sprintf("\n\n<task>\n%s\n</task>", task)
	if expectedOutput != "" {
		msg += fmt.Sprintf("\n\n<expected_output>\n%s\n</expected_output>", expectedOutput)
	}
	return msg
}

// SubSessionConfig describes how to build and run a child session.
// Both handleTaskTransfer and RunAgent (background agents) use this
// to avoid duplicating session-construction logic. Future callers
// (e.g. skill-as-sub-agent) can use it as well.
type SubSessionConfig struct {
	// Task is the user-facing task description.
	Task string
	// ExpectedOutput is an optional description of what the sub-agent should produce.
	ExpectedOutput string
	// SystemMessage, when non-empty, replaces the default task-based system
	// message. This is used by skill sub-agents whose system prompt is the
	// skill content itself rather than the team delegation boilerplate.
	SystemMessage string
	// AgentName is the name of the agent that will execute the sub-session.
	AgentName string
	// Title is a human-readable label for the sub-session (e.g. "Transferred task").
	Title string
	// ToolsApproved overrides whether tools are pre-approved in the child session.
	ToolsApproved bool
	// PinAgent, when true, pins the child session to AgentName via
	// session.WithAgentName. This is required for concurrent background
	// tasks that must not share the runtime's mutable currentAgent field.
	PinAgent bool
	// ImplicitUserMessage, when non-empty, overrides the default "Please proceed."
	// user message sent to the child session. This allows callers like skill
	// sub-agents to pass the task description as the user message.
	ImplicitUserMessage string
	// ExcludedTools lists tool names that should be filtered out of the agent's
	// tool list for the child session. This prevents recursive tool calls
	// (e.g. run_skill calling itself in a skill sub-session).
	ExcludedTools []string
}

// newSubSession builds a *session.Session from a SubSessionConfig and a parent
// session. It consolidates the session options that were previously duplicated
// across handleTaskTransfer and RunAgent.
func newSubSession(parent *session.Session, cfg SubSessionConfig, childAgent *agent.Agent) *session.Session {
	sysMsg := cfg.SystemMessage
	if sysMsg == "" {
		sysMsg = buildTaskSystemMessage(cfg.Task, cfg.ExpectedOutput)
	}

	userMsg := cfg.ImplicitUserMessage
	if userMsg == "" {
		userMsg = "Please proceed."
	}

	opts := []session.Opt{
		session.WithSystemMessage(sysMsg),
		session.WithImplicitUserMessage(userMsg),
		session.WithMaxIterations(childAgent.MaxIterations()),
		session.WithMaxConsecutiveToolCalls(childAgent.MaxConsecutiveToolCalls()),
		session.WithMaxOldToolCallTokens(childAgent.MaxOldToolCallTokens()),
		session.WithTitle(cfg.Title),
		session.WithToolsApproved(cfg.ToolsApproved),
		session.WithSendUserMessage(false),
		session.WithParentID(parent.ID),
	}
	if cfg.PinAgent {
		opts = append(opts, session.WithAgentName(cfg.AgentName))
	}
	// Merge parent's excluded tools with config's excluded tools so that
	// nested sub-sessions (e.g. skill → transfer_task → child) inherit
	// exclusions from all ancestors and don't re-introduce filtered tools.
	excludedTools := mergeExcludedTools(parent.ExcludedTools, cfg.ExcludedTools)
	if len(excludedTools) > 0 {
		opts = append(opts, session.WithExcludedTools(excludedTools))
	}
	return session.New(opts...)
}

// mergeExcludedTools combines two excluded-tool lists, deduplicating entries.
// It returns nil when both inputs are empty.
func mergeExcludedTools(parent, child []string) []string {
	if len(parent) == 0 {
		return child
	}
	if len(child) == 0 {
		return parent
	}
	set := make(map[string]struct{}, len(parent)+len(child))
	for _, t := range parent {
		set[t] = struct{}{}
	}
	for _, t := range child {
		set[t] = struct{}{}
	}
	merged := make([]string, 0, len(set))
	for t := range set {
		merged = append(merged, t)
	}
	return merged
}

// runSubSessionForwarding runs a child session within the parent, forwarding all
// events to the caller's event channel and propagating tool approval state
// back to the parent when done.
//
// This is the "interactive" path used by transfer_task where the parent agent
// loop is blocked while the child executes.
func (r *LocalRuntime) runSubSessionForwarding(ctx context.Context, parent, child *session.Session, span trace.Span, evts chan Event, callerAgent string) (*tools.ToolCallResult, error) {
	childEvents := r.RunStream(ctx, child)
	for event := range childEvents {
		evts <- event
		if errEvent, ok := event.(*ErrorEvent); ok {
			// Drain remaining events (including StreamStoppedEvent) so the
			// TUI's streamDepth counter stays balanced.
			for remaining := range childEvents {
				evts <- remaining
			}
			span.RecordError(fmt.Errorf("%s", errEvent.Error))
			span.SetStatus(codes.Error, "sub-session error")
			return nil, fmt.Errorf("%s", errEvent.Error)
		}
	}

	parent.ToolsApproved = child.ToolsApproved

	parent.AddSubSession(child)
	evts <- SubSessionCompleted(parent.ID, child, callerAgent)

	span.SetStatus(codes.Ok, "sub-session completed")
	return tools.ResultSuccess(child.GetLastAssistantMessageContent()), nil
}

// runSubSessionCollecting runs a child session, collecting output via an
// optional content callback instead of forwarding events. This is the path
// used by delegations and other non-interactive callers.
//
// It returns a delegation.RunResult containing either the final assistant message or
// an error/stopped indication. The ChildSession field is set to the child session
// so the caller can emit SubSessionCompletedEvent.
func (r *LocalRuntime) runSubSessionCollecting(ctx context.Context, parent, child *session.Session, onContent func(string)) *delegation.RunResult {
	var errMsg string
	events := r.RunStream(ctx, child)
	for event := range events {
		if ctx.Err() != nil {
			break
		}
		if choice, ok := event.(*AgentChoiceEvent); ok && choice.Content != "" {
			if onContent != nil {
				onContent(choice.Content)
			}
		}
		if errEvt, ok := event.(*ErrorEvent); ok {
			errMsg = errEvt.Error
			break
		}
	}
	// Drain remaining events so the RunStream goroutine can complete
	// and close the channel without blocking on a full buffer.
	for range events {
	}

	// If the context was cancelled, report stopped rather than failed
	if ctx.Err() != nil {
		return &delegation.RunResult{Stopped: true}
	}

	if errMsg != "" {
		return &delegation.RunResult{ErrMsg: errMsg}
	}

	result := child.GetLastAssistantMessageContent()
	parent.AddSubSession(child)
	return &delegation.RunResult{Result: result, ChildSession: child}
}

// currentDelegationID returns the parent delegation ID for nested delegation creation.
func currentDelegationID(ctx context.Context) string {
	if v, ok := ctx.Value(delegation.ContextKeyDelegationID).(string); ok {
		return v
	}
	return ""
}

// startDelegation prepares a delegation, emits lifecycle events before any blocking sync
// launch, pins the parent event sender for async completion, then launches it.
func (r *LocalRuntime) startDelegation(ctx context.Context, sess *session.Session, evts chan Event, agentName, task, expectedOutput string, mode delegation.DelegationMode) (*delegation.Delegation, error) {
	parentDelegationID := currentDelegationID(ctx)
	d, err := r.delegations.Prepare(sess, parentDelegationID, agentName, task, expectedOutput, mode)
	if err != nil {
		return nil, err
	}
	if evts != nil {
		d.Events = func(event any) bool {
			rtEvent, ok := event.(Event)
			if !ok {
				return false
			}
			select {
			case evts <- rtEvent:
				return true
			default:
				return false
			}
		}
		if !trySendEvent(ctx, evts, DelegationStarted(d.ID, d.ParentDelegationID, sess.ID, d.AgentName, d.Task, string(d.Mode))) {
			return nil, ctx.Err()
		}
		if !trySendEvent(ctx, evts, DelegationTree(r.delegations.Tree(), r.CurrentAgentName())) {
			return nil, ctx.Err()
		}
	}
	if err := r.delegations.Launch(ctx, d); err != nil {
		return nil, err
	}
	return d, nil
}

// CurrentAgentSubAgentNames returns the current agent's sub-agent names
// for backward compatibility with the old agenttool.Runner interface.
func (r *LocalRuntime) CurrentAgentSubAgentNames() []string {
	a := r.CurrentAgent()
	if a == nil {
		return nil
	}
	return agentNames(a.SubAgents())
}

// handleDelegate is the unified handler for the new 'delegate' tool, which
// supports async (default), sync, and handoff delegation modes.
func (r *LocalRuntime) handleDelegate(ctx context.Context, sess *session.Session, toolCall tools.ToolCall, evts chan Event) (*tools.ToolCallResult, error) {
	var params builtin.DelegateArgs
	if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	if params.Agent == "" {
		return tools.ResultError("agent name must not be empty"), nil
	}
	if params.Task == "" {
		return tools.ResultError("task must not be empty"), nil
	}

	a := r.CurrentAgent()

	// Validate agent against sub-agents list
	if errResult := validateAgentInList(a.Name(), params.Agent, "delegate to", "sub-agents list", a.SubAgents()); errResult != nil {
		return errResult, nil
	}

	// Determine delegation mode
	mode := delegation.ModeAsyncDelegate
	switch params.Mode {
	case builtin.DelegateModeSync:
		mode = delegation.ModeSyncDelegate
	case builtin.DelegateModeHandoff:
		mode = delegation.ModeHandoff
	case builtin.DelegateModeAsync, "":
		mode = delegation.ModeAsyncDelegate
	}

	// For handoff mode, use the legacy handoff mechanism
	if mode == delegation.ModeHandoff {
		return r.performHandoff(ctx, sess, params.Agent, evts)
	}

	// Use startDelegation which emits DelegationStarted/Tree events BEFORE blocking sync launch
	// and pins the events channel for async completion delivery.
	d, err := r.startDelegation(ctx, sess, evts, params.Agent, params.Task, params.ExpectedOutput, mode)
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}

	if mode == delegation.ModeSyncDelegate {
		// Emit sub-session completion so PersistentRuntime can persist it
		if d.ChildSession != nil {
			evts <- SubSessionCompleted(sess.ID, d.ChildSession, a.Name())
		}
		// Sync: return result inline
		switch d.LoadStatus() {
		case delegation.StatusFailed:
			return nil, fmt.Errorf("%s", d.ErrMsg)
		case delegation.StatusStopped:
			return tools.ResultError("delegation stopped"), nil
		default:
			return tools.ResultSuccess(d.Result), nil
		}
	}

	// Async: return delegation ID immediately
	return tools.ResultSuccess(fmt.Sprintf("Delegation started with ID: %s\nAgent: %s\nTask: %s\n\nThe agent is running in the background. You will be notified when complete.",
		d.ID, params.Agent, params.Task)), nil
}

// handleListDelegations handles the list_delegations tool call
func (r *LocalRuntime) handleListDelegations(_ context.Context, _ *session.Session, _ tools.ToolCall, _ chan Event) (*tools.ToolCallResult, error) {
	return tools.ResultSuccess(r.delegations.List()), nil
}

// handleViewDelegation handles the view_delegation tool call
func (r *LocalRuntime) handleViewDelegation(_ context.Context, _ *session.Session, toolCall tools.ToolCall, _ chan Event) (*tools.ToolCallResult, error) {
	var params builtin.ViewDelegationArgs
	if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	output, err := r.delegations.View(params.DelegationID)
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}
	return tools.ResultSuccess(output), nil
}

// handleStopDelegation handles the stop_delegation tool call
func (r *LocalRuntime) handleStopDelegation(_ context.Context, _ *session.Session, toolCall tools.ToolCall, _ chan Event) (*tools.ToolCallResult, error) {
	var params builtin.StopDelegationArgs
	if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	if err := r.delegations.Stop(params.DelegationID); err != nil {
		return tools.ResultError(err.Error()), nil
	}
	return tools.ResultSuccess(fmt.Sprintf("Delegation %s stopped.", params.DelegationID)), nil
}

// --- Backward-compatible legacy tool handlers ---

// handleRunBackgroundAgent is an alias for async delegate, preserving
// backward compatibility for run_background_agent tool calls.
func (r *LocalRuntime) handleRunBackgroundAgent(ctx context.Context, sess *session.Session, toolCall tools.ToolCall, evts chan Event) (*tools.ToolCallResult, error) {
	var params struct {
		Agent          string `json:"agent"`
		Task           string `json:"task"`
		ExpectedOutput string `json:"expected_output"`
	}
	if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	if params.Agent == "" {
		return tools.ResultError("agent name must not be empty"), nil
	}
	if params.Task == "" {
		return tools.ResultError("task must not be empty"), nil
	}

	a := r.CurrentAgent()
	if errResult := validateAgentInList(a.Name(), params.Agent, "run background agent", "sub-agents list", a.SubAgents()); errResult != nil {
		return errResult, nil
	}

	d, err := r.startDelegation(ctx, sess, evts, params.Agent, params.Task, params.ExpectedOutput, delegation.ModeAsyncDelegate)
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}

	return tools.ResultSuccess(fmt.Sprintf("Background agent task started with ID: %s\nAgent: %s\nTask: %s",
		d.ID, params.Agent, params.Task)), nil
}

// handleListBackgroundAgents is an alias for list_delegations
func (r *LocalRuntime) handleListBackgroundAgents(_ context.Context, _ *session.Session, _ tools.ToolCall, _ chan Event) (*tools.ToolCallResult, error) {
	return tools.ResultSuccess(r.delegations.List()), nil
}

// handleViewBackgroundAgent is an alias for view_delegation
func (r *LocalRuntime) handleViewBackgroundAgent(_ context.Context, _ *session.Session, toolCall tools.ToolCall, _ chan Event) (*tools.ToolCallResult, error) {
	var params struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	output, err := r.delegations.View(params.TaskID)
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}
	return tools.ResultSuccess(output), nil
}

// handleStopBackgroundAgent is an alias for stop_delegation
func (r *LocalRuntime) handleStopBackgroundAgent(_ context.Context, _ *session.Session, toolCall tools.ToolCall, _ chan Event) (*tools.ToolCallResult, error) {
	var params struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	if err := r.delegations.Stop(params.TaskID); err != nil {
		return tools.ResultError(err.Error()), nil
	}
	return tools.ResultSuccess(fmt.Sprintf("Background agent task %s stopped.", params.TaskID)), nil
}

// performHandoff handles the legacy handoff behavior: switches currentAgent.
func (r *LocalRuntime) performHandoff(_ context.Context, _ *session.Session, targetAgent string, _ chan Event) (*tools.ToolCallResult, error) {
	ca := r.CurrentAgentName()
	currentAgent, err := r.team.Agent(ca)
	if err != nil {
		return nil, fmt.Errorf("current agent not found: %w", err)
	}

	if errResult := validateAgentInList(ca, targetAgent, "hand off to", "handoffs list", currentAgent.Handoffs()); errResult != nil {
		return errResult, nil
	}

	next, err := r.team.Agent(targetAgent)
	if err != nil {
		return nil, err
	}

	r.setCurrentAgent(next.Name())
	handoffMessage := "The agent " + ca + " handed off the conversation to you. " +
		"Your available handoff agents and tools are specified in the system messages that follow. " +
		"Only use those capabilities - do not attempt to use tools or hand off to agents that you see " +
		"in the conversation history from previous agents, as those were available to different agents " +
		"with different capabilities. Look at the conversation history for context, but only use the " +
		"handoff agents and tools that are listed in your system messages below. " +
		"Complete your part of the task and hand off to the next appropriate agent in your workflow " +
		"(if any are available to you), or respond directly to the user if you are the final agent."
	return tools.ResultSuccess(handoffMessage), nil
}

// RunDelegation implements delegation.DelegationRunner. It starts a sub-agent
// and blocks until completion or cancellation, returning the delegation result.
func (r *LocalRuntime) RunDelegation(ctx context.Context, params delegation.RunParams) *delegation.RunResult {
	child, err := r.team.Agent(params.AgentName)
	if err != nil {
		return &delegation.RunResult{ErrMsg: fmt.Sprintf("agent %q not found: %s", params.AgentName, err)}
	}

	sess := params.ParentSession

	// Delegations run with tools pre-approved because there is no user present
	// to respond to interactive approval prompts during async execution.
	// This is a deliberate design trade-off: the user implicitly authorises all tool calls made
	// by the sub-agent when they approve the delegation. Callers should be aware
	// that prompt injection in the sub-agent's context could exploit this gate-bypass.
	cfg := SubSessionConfig{
		Task:           params.Task,
		ExpectedOutput: params.ExpectedOutput,
		AgentName:      params.AgentName,
		Title:          "Delegated task",
		ToolsApproved:  true,
		PinAgent:       true,
	}

	s := newSubSession(sess, cfg, child)

	return r.runSubSessionCollecting(ctx, sess, s, params.OnContent)
}

func (r *LocalRuntime) handleTaskTransfer(ctx context.Context, sess *session.Session, toolCall tools.ToolCall, evts chan Event) (*tools.ToolCallResult, error) {
	var params struct {
		Agent          string `json:"agent"`
		Task           string `json:"task"`
		ExpectedOutput string `json:"expected_output"`
	}

	if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	a := r.CurrentAgent()

	// Validate that the target agent is in the current agent's sub-agents list
	if errResult := validateAgentInList(a.Name(), params.Agent, "transfer task to", "sub-agents list", a.SubAgents()); errResult != nil {
		return errResult, nil
	}

	ctx, span := r.startSpan(ctx, "runtime.task_transfer", trace.WithAttributes(
		attribute.String("from.agent", a.Name()),
		attribute.String("to.agent", params.Agent),
		attribute.String("session.id", sess.ID),
	))
	defer span.End()

	slog.Debug("Transferring task to agent", "from_agent", a.Name(), "to_agent", params.Agent, "task", params.Task)

	// Use startDelegation which emits DelegationStarted/Tree events BEFORE blocking sync launch
	// and pins the events channel for async completion delivery.
	d, err := r.startDelegation(ctx, sess, evts, params.Agent, params.Task, params.ExpectedOutput, delegation.ModeSyncDelegate)
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}

	// Emit sub-session completion so PersistentRuntime can persist it
	if d.ChildSession != nil {
		evts <- SubSessionCompleted(sess.ID, d.ChildSession, a.Name())
	}

	if d.LoadStatus() == delegation.StatusFailed {
		span.RecordError(fmt.Errorf("%s", d.ErrMsg))
		span.SetStatus(codes.Error, "delegation failed")
		return nil, fmt.Errorf("%s", d.ErrMsg)
	}
	if d.LoadStatus() == delegation.StatusStopped {
		span.SetStatus(codes.Ok, "delegation stopped")
		return tools.ResultError("delegation stopped"), nil
	}

	span.SetStatus(codes.Ok, "delegation completed")
	return tools.ResultSuccess(d.Result), nil
}

func (r *LocalRuntime) handleHandoff(_ context.Context, _ *session.Session, toolCall tools.ToolCall, _ chan Event) (*tools.ToolCallResult, error) {
	var params builtin.HandoffArgs
	if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	ca := r.CurrentAgentName()
	currentAgent, err := r.team.Agent(ca)
	if err != nil {
		return nil, fmt.Errorf("current agent not found: %w", err)
	}

	// Validate that the target agent is in the current agent's handoffs list
	if errResult := validateAgentInList(ca, params.Agent, "hand off to", "handoffs list", currentAgent.Handoffs()); errResult != nil {
		return errResult, nil
	}

	next, err := r.team.Agent(params.Agent)
	if err != nil {
		return nil, err
	}

	r.setCurrentAgent(next.Name())
	handoffMessage := "The agent " + ca + " handed off the conversation to you. " +
		"Your available handoff agents and tools are specified in the system messages that follow. " +
		"Only use those capabilities - do not attempt to use tools or hand off to agents that you see " +
		"in the conversation history from previous agents, as those were available to different agents " +
		"with different capabilities. Look at the conversation history for context, but only use the " +
		"handoff agents and tools that are listed in your system messages below. " +
		"Complete your part of the task and hand off to the next appropriate agent in your workflow " +
		"(if any are available to you), or respond directly to the user if you are the final agent."
	return tools.ResultSuccess(handoffMessage), nil
}
