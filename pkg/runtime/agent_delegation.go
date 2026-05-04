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
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin"
	agenttool "github.com/docker/docker-agent/pkg/tools/builtin/agent"
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
// loop is blocked while the child executes. The child session is driven through
// this runner so nested child-launched sub-sessions inherit the invoking
// runner's sessionState rather than falling back to the root runtime's state.
func (sr *sessionRunner) runSubSessionForwarding(ctx context.Context, parent, child *session.Session, span trace.Span, evts chan Event, callerAgent string) (*tools.ToolCallResult, error) {
	childEvents := sr.runStreamWithConfig(ctx, sessionRunConfig{
		sess:   child,
		policy: rootWakePolicy{runner: sr},
	})
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
// used by background agents and other non-interactive callers.
//
// It returns a RunResult containing either the final assistant message or
// an error message. Like runSubSessionForwarding, the child is driven through
// the invoking runner so nested child-launched sessions reuse the correct
// sessionState.
func (sr *sessionRunner) runSubSessionCollecting(ctx context.Context, parent, child *session.Session, onContent func(string)) *agenttool.RunResult {
	var errMsg string
	events := sr.runStreamWithConfig(ctx, sessionRunConfig{
		sess:   child,
		policy: rootWakePolicy{runner: sr},
	})
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

	if errMsg != "" {
		return &agenttool.RunResult{ErrMsg: errMsg}
	}

	result := child.GetLastAssistantMessageContent()
	parent.AddSubSession(child)
	return &agenttool.RunResult{Result: result}
}

func (r *LocalRuntime) CurrentAgentSubAgentNames() []string {
	a := r.CurrentAgent()
	if a == nil {
		return nil
	}
	return agentNames(a.SubAgents())
}

func (r *LocalRuntime) RunAgent(ctx context.Context, params agenttool.RunParams) *agenttool.RunResult {
	child, err := r.team.Agent(params.AgentName)
	if err != nil {
		return &agenttool.RunResult{ErrMsg: fmt.Sprintf("agent %q not found: %s", params.AgentName, err)}
	}

	sess := params.ParentSession

	// Background tasks run with tools pre-approved because there is no user present
	// to respond to interactive approval prompts during async execution. This is a
	// deliberate design trade-off: the user implicitly authorises all tool calls made
	// by the sub-agent when they approve run_background_agent. Callers should be aware
	// that prompt injection in the sub-agent's context could exploit this gate-bypass.
	//
	// TODO: propagate the parent session's per-tool permission rules once the runtime
	// supports per-session permission scoping rather than a single shared ToolsApproved flag.
	cfg := SubSessionConfig{
		Task:           params.Task,
		ExpectedOutput: params.ExpectedOutput,
		AgentName:      params.AgentName,
		Title:          "Background agent task",
		ToolsApproved:  true,
		PinAgent:       true,
	}

	s := newSubSession(sess, cfg, child)

	return newRootSessionRunner(r).runSubSessionCollecting(ctx, sess, s, params.OnContent)
}

func (r *LocalRuntime) handleTaskTransfer(ctx context.Context, sr *sessionRunner, sess *session.Session, toolCall tools.ToolCall, evts chan Event) (*tools.ToolCallResult, error) {
	var params struct {
		Agent          string `json:"agent"`
		Task           string `json:"task"`
		ExpectedOutput string `json:"expected_output"`
	}

	if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	a := r.resolveSessionAgent(sess)

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

	// Emit agent switching start event
	evts <- AgentSwitching(true, a.Name(), params.Agent)

	// Scope the agent-switch mutation correctly:
	//   - For root sessions, mutate the runtime's global current-agent selection
	//     so resolveSessionAgent (which falls back to it) returns params.Agent for
	//     the unpinned child session created below.
	//   - For sub-sessions, do not mutate the parent session's AgentName at all.
	//     The parent keeps its own pinned identity while the newly created child
	//     sub-session is pinned to params.Agent via PinAgent below.
	isSubSession := sess != nil && sess.IsSubSession()
	if isSubSession {
		defer func() {
			// Emit agent switching end event
			evts <- AgentSwitching(false, params.Agent, a.Name())
			// Restore original agent info in sidebar
			evts <- AgentInfo(a.Name(), getAgentModelID(a), a.Description(), a.WelcomeMessage())
		}()
	} else {
		r.setCurrentAgent(params.Agent)
		defer func() {
			r.setCurrentAgent(a.Name())
			// Emit agent switching end event
			evts <- AgentSwitching(false, params.Agent, a.Name())
			// Restore original agent info in sidebar
			evts <- AgentInfo(a.Name(), getAgentModelID(a), a.Description(), a.WelcomeMessage())
		}()
	}

	// Emit agent info for the new agent
	child, err := r.team.Agent(params.Agent)
	if err != nil {
		return nil, err
	}
	evts <- AgentInfo(child.Name(), getAgentModelID(child), child.Description(), child.WelcomeMessage())

	slog.Debug("Creating new session with parent session", "parent_session_id", sess.ID, "tools_approved", sess.ToolsApproved)

	cfg := SubSessionConfig{
		Task:           params.Task,
		ExpectedOutput: params.ExpectedOutput,
		AgentName:      params.Agent,
		Title:          "Transferred task",
		ToolsApproved:  sess.ToolsApproved,
		// Pin every transferred child session to the resolved target agent. Root
		// sessions still temporarily switch the runtime's current agent for legacy
		// UI/event behaviour, but the child session itself should resolve entirely
		// via its own AgentName.
		PinAgent: true,
	}

	s := newSubSession(sess, cfg, child)

	return sr.runSubSessionForwarding(ctx, sess, s, span, evts, a.Name())
}

func (r *LocalRuntime) handleHandoff(_ context.Context, _ *sessionRunner, sess *session.Session, toolCall tools.ToolCall, _ chan Event) (*tools.ToolCallResult, error) {
	// Handoff mutates the runtime-wide root current-agent selection. Child
	// sessions are pinned to their own agent and may run concurrently with the
	// root, so allowing a child to call handoff would corrupt root state and
	// violate session-local execution. Subagents should use their own configured
	// tools, not root handoff routing.
	if sess != nil && sess.IsSubSession() {
		return tools.ResultError("handoff is only available from the root session; subagent sessions cannot mutate the root agent selection"), nil
	}

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
