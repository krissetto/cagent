package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
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
func validateAgentInList(currentAgent, targetAgent, action, listDesc string, agents []*agent.Agent) *tools.ToolCallResult {
	for _, a := range agents {
		if a.Name() == targetAgent {
			return nil
		}
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

// SubSessionConfig describes how to build and run a child session.
type SubSessionConfig struct {
	Task                string
	ExpectedOutput      string
	SystemMessage       string
	AgentName           string
	Title               string
	ToolsApproved       bool
	PinAgent            bool
	ImplicitUserMessage string
	ExcludedTools       []string
}

// newSubSession builds a *session.Session from a SubSessionConfig and a parent session.
func newSubSession(parent *session.Session, cfg SubSessionConfig, childAgent *agent.Agent) *session.Session {
	sysMsg := cfg.SystemMessage
	if sysMsg == "" {
		// Use the child agent's own configured system messages, not a synthetic prompt
		sysMsg = ""
	}

	userMsg := cfg.ImplicitUserMessage
	if userMsg == "" {
		userMsg = "Please proceed."
	}

	opts := []session.Opt{
		session.WithImplicitUserMessage(userMsg),
		session.WithMaxIterations(childAgent.MaxIterations()),
		session.WithMaxConsecutiveToolCalls(childAgent.MaxConsecutiveToolCalls()),
		session.WithMaxOldToolCallTokens(childAgent.MaxOldToolCallTokens()),
		session.WithTitle(cfg.Title),
		session.WithToolsApproved(cfg.ToolsApproved),
		session.WithSendUserMessage(false),
		session.WithParentID(parent.ID),
	}
	if sysMsg != "" {
		opts = append(opts, session.WithSystemMessage(sysMsg))
	}
	if cfg.PinAgent {
		opts = append(opts, session.WithAgentName(cfg.AgentName))
	}
	excludedTools := mergeExcludedTools(parent.ExcludedTools, cfg.ExcludedTools)
	if len(excludedTools) > 0 {
		opts = append(opts, session.WithExcludedTools(excludedTools))
	}
	return session.New(opts...)
}

// mergeExcludedTools combines two excluded-tool lists, deduplicating entries.
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

// runSubSessionForwarding runs a child session and forwards all events to the parent's event channel.
func (r *LocalRuntime) runSubSessionForwarding(ctx context.Context, parent, child *session.Session, span trace.Span, evts chan Event, callerAgent string) (*tools.ToolCallResult, error) {
	childEvents := r.RunStream(ctx, child)
	for event := range childEvents {
		evts <- event
		if errEvent, ok := event.(*ErrorEvent); ok {
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

// RunDelegation implements delegation.DelegationRunner.
//
// It is a pure executor: it receives a fully-composed childSess and runs it,
// returning the last assistant message. It does NOT add messages to childSess
// or perform whole-session persistence — that is the caller's responsibility
// (Manager.Start / Manager.Continue).
//
// The context should carry delegation.BackgroundRunKey when invoked from a
// background goroutine (via Manager.Start or Manager.Continue) so that RunStream
// skips the global elicitation channel swap — preventing concurrent child runs
// from clobbering each other.
func (r *LocalRuntime) RunDelegation(ctx context.Context, d *delegation.Delegation, childSess *session.Session) (string, error) {
	// Ensure child session row exists before incremental message persistence.
	if r.sessionStore != nil {
		if err := r.sessionStore.UpdateSession(ctx, childSess); err != nil {
			slog.Warn("Failed to upsert child session before delegation run",
				"session_id", childSess.ID, "error", err)
		}
		// Persist any messages in childSess that are not yet in the store.
		// SQL-backed stores only write session metadata in UpdateSession; in-memory
		// stores include the full message list. We detect the gap by comparing the
		// stored count to the in-memory count and persisting the delta — so this is
		// a no-op for in-memory (already included) and correctly persists the initial
		// task (Start) or follow-up message (Continue) for SQL-backed stores.
		if storedSess, getErr := r.sessionStore.GetSession(ctx, childSess.ID); getErr == nil {
			storedCount := len(storedSess.Messages)
			for i := storedCount; i < len(childSess.Messages); i++ {
				item := childSess.Messages[i]
				if item.Message != nil {
					if _, addErr := r.sessionStore.AddMessage(ctx, childSess.ID, item.Message); addErr != nil {
						slog.Warn("Failed to persist pre-run session message",
							"session_id", childSess.ID, "position", i, "error", addErr)
					}
				}
			}
		}
	}

	// Clear any previous closed latch so that a continuation can establish
	// fresh subscriptions and receive live events again via the event bus.
	r.delegationEventBus.Reopen(childSess.ID)

	events := r.RunStream(ctx, childSess)
	var lastAssistant string
	var errMsg string
	var streaming streamingState // tracks incremental persistence state
	for event := range events {
		// Publish every child event to the delegation event bus so TUI
		// subscribers can receive live updates.
		r.delegationEventBus.Publish(childSess.ID, event)

		// Incrementally persist child events if a store is configured.
		if r.sessionStore != nil {
			persistChildEvent(ctx, r.sessionStore, childSess.ID, event, &streaming)
		}

		switch e := event.(type) {
		case *AgentChoiceEvent:
			if e.SessionID == childSess.ID && e.Content != "" {
				lastAssistant += e.Content
			}
		case *MessageAddedEvent:
			if e.SessionID == childSess.ID && e.Message != nil && e.Message.Message.Role == chat.MessageRoleAssistant {
				lastAssistant = strings.TrimSpace(e.Message.Message.Content)
			}
		case *ErrorEvent:
			errMsg = e.Error
		}
	}

	// Generate a title for the child session from the first user message
	// (the parent agent's task). This provides a meaningful label in the
	// sidebar and tab header.
	if childSess.Title == "" {
		for _, item := range childSess.Messages {
			if item.Message != nil && item.Message.Message.Role == chat.MessageRoleUser && !item.Message.Implicit {
				title := item.Message.Message.Content
				if len(title) > 60 {
					title = title[:57] + "..."
				}
				childSess.Title = title
				if r.sessionStore != nil {
					_ = r.sessionStore.UpdateSession(ctx, childSess)
				}
				r.delegationEventBus.Publish(childSess.ID, SessionTitle(childSess.ID, title))
				break
			}
		}
	}

	// Signal subscribers that the child session stream is done.
	r.delegationEventBus.Close(childSess.ID)

	if errMsg != "" {
		return "", fmt.Errorf("%s", errMsg)
	}

	return lastAssistant, nil
}

// handleDelegate starts a new background delegation and returns immediately with
// {"delegation_id":"<id>","status":"started"}. The child agent runs asynchronously;
// use continue_delegation to send follow-up messages or stop_delegation to cancel.
func (r *LocalRuntime) handleDelegate(ctx context.Context, sess *session.Session, toolCall tools.ToolCall, evts chan Event) (*tools.ToolCallResult, error) {
	var params builtin.DelegateArgs
	if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if strings.TrimSpace(params.Agent) == "" {
		return tools.ResultError("agent is required"), nil
	}
	if strings.TrimSpace(params.Task) == "" {
		return tools.ResultError("task is required"), nil
	}

	a := r.CurrentAgent()
	if errResult := validateAgentInList(a.Name(), params.Agent, "delegate to", "sub-agents list", a.SubAgents()); errResult != nil {
		return errResult, nil
	}

	delegationID, sessionID, err := r.delegations.Start(ctx, delegation.StartParams{
		AgentName:       params.Agent,
		Task:            params.Task,
		ParentSessionID: sess.ID,
		ParentSession:   sess,
		WorkingDir:      sess.WorkingDir,
	})
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}

	// Emit DelegationStarted immediately so the TUI can show the card.
	if evts != nil {
		trySendEvent(ctx, evts, DelegationStarted(delegationID, sessionID, sess.ID, params.Agent, params.Task))
	}

	return tools.ResultSuccess(fmt.Sprintf(`{"delegation_id":%q,"status":"started"}`, delegationID)), nil
}

func (r *LocalRuntime) handleContinueDelegation(ctx context.Context, sess *session.Session, toolCall tools.ToolCall, evts chan Event) (*tools.ToolCallResult, error) {
	var params builtin.ContinueDelegationArgs
	if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if strings.TrimSpace(params.DelegationID) == "" {
		return tools.ResultError("delegation_id is required"), nil
	}
	if strings.TrimSpace(params.Message) == "" {
		return tools.ResultError("message is required"), nil
	}

	// Look up the delegation so we can emit lifecycle events with the child session ID.
	d, ok := r.delegations.Get(params.DelegationID)
	if !ok {
		return tools.ResultError("delegation not found"), nil
	}

	// Reactivate the delegation card in the TUI before the async run.
	if evts != nil {
		trySendEvent(ctx, evts, DelegationStarted(params.DelegationID, d.SessionID, sess.ID, d.AgentName, params.Message))
	}

	err := r.delegations.Continue(ctx, params.DelegationID, params.Message)
	if err != nil {
		if evts != nil {
			trySendEvent(ctx, evts, DelegationFailed(params.DelegationID, d.SessionID, sess.ID, d.AgentName, err.Error()))
		}
		return tools.ResultError(err.Error()), nil
	}

	// continue_delegation is now async — reply arrives via completion callback.
	return tools.ResultSuccess(fmt.Sprintf(`{"delegation_id":%q,"status":"message_sent"}`, params.DelegationID)), nil
}

func (r *LocalRuntime) handleStopDelegation(ctx context.Context, _ *session.Session, toolCall tools.ToolCall, _ chan Event) (*tools.ToolCallResult, error) {
	var params builtin.StopDelegationArgs
	if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if params.DelegationID == "" {
		return tools.ResultError("delegation_id must not be empty"), nil
	}
	if err := r.delegations.Stop(ctx, params.DelegationID); err != nil {
		return tools.ResultError(err.Error()), nil
	}
	// DelegationStopped lifecycle event is emitted by the onCompletion callback
	// once the background goroutine actually terminates — not synchronously here.
	// This prevents double-firing and ensures the event fires only after the child
	// session has fully stopped.
	return tools.ResultSuccess("delegation stopped"), nil
}

func (r *LocalRuntime) handleGetDelegationResult(ctx context.Context, _ *session.Session, toolCall tools.ToolCall, _ chan Event) (*tools.ToolCallResult, error) {
	var params builtin.GetDelegationResultArgs
	if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if strings.TrimSpace(params.DelegationID) == "" {
		return tools.ResultError("delegation_id is required"), nil
	}

	d, ok := r.delegations.Get(params.DelegationID)
	if !ok {
		return tools.ResultError(fmt.Sprintf("delegation %s not found", params.DelegationID)), nil
	}

	status := d.LoadStatus().String()
	lastReply := d.GetLastReply()
	var errStr string
	if e := d.GetError(); e != nil {
		errStr = e.Error()
	}

	result := fmt.Sprintf(
		`{"delegation_id":%q,"session_id":%q,"agent":%q,"status":%q,"last_reply":%q,"error":%q}`,
		params.DelegationID, d.SessionID, d.AgentName, status, lastReply, errStr)
	return tools.ResultSuccess(result), nil
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
