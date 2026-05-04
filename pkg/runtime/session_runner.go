// session_runner.go contains the sessionRunner type — the first-class
// internal per-session driver.
//
// sessionRunner pairs a shared *runtimeCore (team, stores, event bus, …)
// with a fresh *sessionState (per-session channels and flags) and owns
// the session-local tool dispatch table and background-agent handler.
// Both root and child sessions are driven through sessionRunner.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/telemetry"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin"
	agenttool "github.com/docker/docker-agent/pkg/tools/builtin/agent"
)

// sessionRunner is the first-class internal per-session driver. It pairs
// the runtime-wide shared services in *runtimeCore with a session-specific
// *sessionState, plus the per-session tool dispatch table and background-
// agent handler.
//
// Two kinds of sessionRunner exist:
//
//   - Root runners: built via [newRootSessionRunner]. They build a
//     fresh per-session toolMap and reuse the root LocalRuntime's
//     bgAgents handler (so [LocalRuntime.Close] can stop all root
//     background tasks). They use the root sessionState that backs the
//     public Resume / Steer / FollowUp / ResumeElicitation APIs.
//
//   - Child runners: built via [newChildSessionRunner]. They own a
//     freshly-allocated toolMap and bgAgents so child-session tool
//     dispatch is isolated. The root field still points to the root
//     LocalRuntime for methods that are not yet session-local (e.g.
//     hook execution, fallback chain, summarization).
//
// sessionRunner is internal to this package. External callers interact
// through the public Runtime / LiveSessionRuntime interfaces on
// *LocalRuntime.
type sessionRunner struct {
	core     *runtimeCore
	state    *sessionState
	toolMap  map[string]toolHandlerFunc
	bgAgents *agenttool.Handler

	// root is the root *LocalRuntime from which this runner was derived.
	// It provides access to methods that have not been session-localised
	// yet and always points to the same runtime that owns the shared
	// runtimeCore.
	root *LocalRuntime
}

// childAgentRunner adapts background-agent lookups/execution to a specific
// child session by holding a pointer to the invoking *sessionRunner. This
// lets it (a) resolve sub-agent names against the child's session-pinned
// agent rather than the root runtime's mutable current-agent selection, and
// (b) launch background sub-sessions through the child runner so they
// inherit the child's *sessionState (resumeChan, follow-up/steer queues,
// elicitation channels) instead of the root's.
type childAgentRunner struct {
	sr *sessionRunner
}

func (c childAgentRunner) CurrentAgentSubAgentNames() []string {
	a, err := c.sr.root.team.Agent(c.sr.state.currentAgentName())
	if err != nil || a == nil {
		return nil
	}
	return agentNames(a.SubAgents())
}

// RunAgent runs a background agent as a sub-session driven by the child
// runner. Because the new sub-session goes through sr.runSubSessionCollecting
// (which calls sr.runStreamWithConfig directly), it shares the child runner's
// *sessionState rather than the root's. This resolves the previous Phase 1
// limitation where nested sub-sessions launched from a child observed the
// root runtime's coordination channels.
func (c childAgentRunner) RunAgent(ctx context.Context, params agenttool.RunParams) *agenttool.RunResult {
	child, err := c.sr.root.team.Agent(params.AgentName)
	if err != nil {
		return &agenttool.RunResult{ErrMsg: fmt.Sprintf("agent %q not found: %s", params.AgentName, err)}
	}

	sess := params.ParentSession

	cfg := SubSessionConfig{
		Task:           params.Task,
		ExpectedOutput: params.ExpectedOutput,
		AgentName:      params.AgentName,
		Title:          "Background agent task",
		ToolsApproved:  true,
		PinAgent:       true,
	}

	s := newSubSession(sess, cfg, child)
	return c.sr.runSubSessionCollecting(ctx, sess, s, params.OnContent)
}

// newRootSessionRunner creates a sessionRunner for the root session of r.
// It builds the root runner's session-local toolMap and reuses r's bgAgents
// so runtime shutdown can still stop all root background tasks via
// [LocalRuntime.Close].
func newRootSessionRunner(r *LocalRuntime) *sessionRunner {
	return &sessionRunner{
		core:     r.runtimeCore,
		state:    r.sessionState,
		toolMap:  buildToolMap(r, r.bgAgents),
		bgAgents: r.bgAgents,
		root:     r,
	}
}

// newChildSessionRunner creates a sessionRunner for a subagent child
// session. It shares r's *runtimeCore but owns a fresh *sessionState
// and freshly-registered tool handlers so child-session coordination
// channels and tool dispatch do not collide with root or sibling
// sessions.
//
// Tool handlers are registered once at construction time (via
// registerDefaultToolsInto) so they are safe to read concurrently from
// the engine without synchronisation. Child background-agent handlers do
// not close over the root LocalRuntime directly for agent-list reads;
// they use a childAgentRunner adapter pinned to state/session identity so
// CurrentAgentSubAgentNames resolves against the child agent, not the
// root runtime's mutable current-agent selection.
func newChildSessionRunner(r *LocalRuntime, state *sessionState) *sessionRunner {
	sr := &sessionRunner{
		core:  r.runtimeCore,
		state: state,
		root:  r,
	}
	sr.bgAgents = agenttool.NewHandler(childAgentRunner{sr: sr})
	sr.toolMap = buildToolMap(r, sr.bgAgents)
	return sr
}

// buildToolMap constructs the per-session runtime-managed tool dispatch map.
// Root and child runners both call this during runner construction so the
// toolMap is owned entirely by [sessionRunner] instances.
func buildToolMap(r *LocalRuntime, bgAgents *agenttool.Handler) map[string]toolHandlerFunc {
	toolMap := make(map[string]toolHandlerFunc)
	registerDefaultToolsInto(toolMap, r, bgAgents)
	return toolMap
}

// registerDefaultToolsInto fills toolMap with the runtime-managed tool
// handlers that close over the given root LocalRuntime. Root and child
// runners both call it while building their own per-session toolMap.
func registerDefaultToolsInto(toolMap map[string]toolHandlerFunc, r *LocalRuntime, bgAgents *agenttool.Handler) {
	toolMap[builtin.ToolNameTransferTask] = r.handleTaskTransfer
	toolMap[builtin.ToolNameHandoff] = r.handleHandoff
	toolMap[builtin.ToolNameChangeModel] = r.handleChangeModel
	toolMap[builtin.ToolNameRevertModel] = r.handleRevertModel
	toolMap[builtin.ToolNameRunSkill] = r.handleRunSkill
	toolMap[subagent.ToolNameStart] = r.handleSubagentStart
	toolMap[subagent.ToolNameSend] = r.handleSubagentSend
	toolMap[subagent.ToolNameList] = r.handleSubagentList
	toolMap[subagent.ToolNameInspect] = r.handleSubagentInspect
	toolMap[subagent.ToolNameFinalize] = r.handleSubagentClose
	toolMap[subagent.ToolNameClose] = r.handleSubagentClose
	toolMap[subagent.ToolNameStop] = r.handleSubagentStop
	if bgAgents == nil {
		return
	}

	bgAgents.RegisterHandlers(func(name string, fn func(context.Context, *session.Session, tools.ToolCall) (*tools.ToolCallResult, error)) {
		toolMap[name] = func(ctx context.Context, _ *sessionRunner, sess *session.Session, tc tools.ToolCall, _ chan Event) (*tools.ToolCallResult, error) {
			return fn(ctx, sess, tc)
		}
	})

	// Override run_background_agent with a session-aware validation wrapper.
	//
	// The default agenttool.Handler.HandleRun validates against its Runner's
	// CurrentAgentSubAgentNames(), which is correct for sessions whose
	// allowed sub-agent list matches the runner identity. A runner can also
	// drive nested sub-sessions pinned to a different agent, though, and in
	// that case validation must use the nested session's allowed sub-agents.
	//
	// Compute the allowed names from r.resolveSessionAgent(sess).SubAgents()
	// and delegate to HandleRunWithAllowedAgents so validation stays
	// session-scoped without mutating shared runner state.
	toolMap[agenttool.ToolNameRunBackgroundAgent] = func(ctx context.Context, _ *sessionRunner, sess *session.Session, tc tools.ToolCall, _ chan Event) (*tools.ToolCallResult, error) {
		a := r.resolveSessionAgent(sess)
		return bgAgents.HandleRunWithAllowedAgents(ctx, sess, tc, agentNames(a.SubAgents()))
	}
}

// ── Session-local elicitation methods ──────────────────────────────────

// swapElicitationEventsChannel atomically replaces the current
// elicitation events channel on this runner's session state and returns
// the previous one. Each runStreamWithConfig call swaps in its own
// channel on entry and swaps the previous one back on exit via
// finalizeEventChannel, so nested streams don't lose the parent's
// channel.
func (sr *sessionRunner) swapElicitationEventsChannel(ch chan Event) chan Event {
	sr.state.elicitationEventsChannelMux.Lock()
	defer sr.state.elicitationEventsChannelMux.Unlock()
	prev := sr.state.elicitationEventsChannel
	sr.state.elicitationEventsChannel = ch
	return prev
}

// elicitationHandlerFor returns the appropriate elicitation handler for
// sess. Non-interactive sessions (unattached subagents, --exec mode)
// receive an auto-decline handler. Interactive (root) sessions receive
// the normal elicitationHandler.
func (sr *sessionRunner) elicitationHandlerFor(sess *session.Session) tools.ElicitationHandler {
	if sess != nil && sess.NonInteractive {
		return sr.autoDeclineElicitationHandler
	}
	return sr.elicitationHandler
}

// autoDeclineElicitationHandler immediately declines MCP elicitation
// requests for non-interactive sessions (e.g. subagents). It emits a
// warning event on the session's event channel so the decline is visible
// to any observer.
func (sr *sessionRunner) autoDeclineElicitationHandler(_ context.Context, req *mcp.ElicitParams) (tools.ElicitationResult, error) {
	cleanMsg := req.Message
	slog.Warn("Elicitation auto-declined: subagent sessions cannot answer MCP elicitations yet",
		"message", cleanMsg)

	sr.state.elicitationEventsChannelMux.RLock()
	ch := sr.state.elicitationEventsChannel
	sr.state.elicitationEventsChannelMux.RUnlock()
	if ch != nil {
		agentName := sr.state.currentAgentName()
		select {
		case ch <- Warning("Elicitation declined: subagent sessions are non-interactive and cannot answer MCP elicitations (no ResumeElicitationByID route to child runtime). Prompt: "+cleanMsg, agentName):
		default:
		}
	}

	return tools.ElicitationResult{Action: tools.ElicitationActionDecline}, nil
}

// elicitationHandler propagates elicitation requests to the runtime's
// client via the session's events channel and blocks until the client
// responds or the context is cancelled. This handler is only used by
// interactive (root) sessions; child sessions use
// autoDeclineElicitationHandler.
func (sr *sessionRunner) elicitationHandler(ctx context.Context, req *mcp.ElicitParams) (tools.ElicitationResult, error) {
	slog.Debug("Elicitation request received from MCP server", "message", req.Message)

	sr.state.elicitationEventsChannelMux.RLock()
	eventsChannel := sr.state.elicitationEventsChannel
	if eventsChannel == nil {
		sr.state.elicitationEventsChannelMux.RUnlock()
		return tools.ElicitationResult{}, errors.New("no events channel available for elicitation")
	}

	sr.root.executeOnUserInputHooks(ctx, sr.root.CurrentAgent(), "", "elicitation")

	slog.Debug("Sending elicitation request event to client",
		"message", req.Message, "mode", req.Mode,
		"requested_schema", req.RequestedSchema, "url", req.URL)
	slog.Debug("Elicitation request meta", "meta", req.Meta)

	eventsChannel <- ElicitationRequest(req.Message, req.Mode, req.RequestedSchema, req.URL, req.ElicitationID, req.Meta, sr.root.CurrentAgentName())
	sr.state.elicitationEventsChannelMux.RUnlock()

	select {
	case result := <-sr.state.elicitationRequestCh:
		return tools.ElicitationResult{
			Action:  result.Action,
			Content: result.Content,
		}, nil
	case <-ctx.Done():
		slog.Debug("Context cancelled while waiting for elicitation response")
		return tools.ElicitationResult{}, ctx.Err()
	}
}

// ── Toolset configuration ──────────────────────────────────────────────

// configureToolsetHandlers sets up elicitation and OAuth handlers for
// all toolsets of an agent. The sess parameter selects the elicitation
// handler: non-interactive sessions use the auto-decline handler.
func (sr *sessionRunner) configureToolsetHandlers(a *agent.Agent, sess *session.Session, events chan Event) {
	for _, toolset := range a.ToolSets() {
		tools.ConfigureHandlers(toolset,
			sr.elicitationHandlerFor(sess),
			func() { events <- Authorization(tools.ElicitationActionAccept, a.Name()) },
			sr.core.managedOAuth,
		)

		if ragTool, ok := tools.As[*builtin.RAGTool](toolset); ok {
			ragTool.SetEventCallback(ragEventForwarder(ragTool.Name(), a.Name(), chanSend(events)))
		}
	}
}

// ── Tool dispatch ──────────────────────────────────────────────────────
//
// processToolCalls lives in tool_dispatch.go (sessionRunner method) so the
// approval/permission helpers it depends on stay co-located.

// ── Subagent envelope injection ────────────────────────────────────────

// injectSubagentEnvelope renders a user-role reminder for the parent
// and records the subagent update as a persistable event.
func (sr *sessionRunner) injectSubagentEnvelope(sess *session.Session, env subagent.Envelope, events chan Event) {
	sr.root.appendSubagentEnvelopeToSession(sess, env, func(ev Event) { events <- ev })
}

// drainSubagentInbox injects all pending child envelopes into the
// parent session as implicit user-role reminders. Returns true when at
// least one envelope was delivered.
func (sr *sessionRunner) drainSubagentInbox(sess *session.Session, events chan Event) bool {
	envs := sr.core.subagents.DrainParentInbox(sess.ID)
	if len(envs) == 0 {
		return false
	}
	for _, env := range envs {
		sr.injectSubagentEnvelope(sess, env, events)
	}
	return true
}

// ── Parent idle-wait ───────────────────────────────────────────────────

// waitForSubagentInbox blocks until one of the following happens:
//
//   - a child envelope becomes available for the parent session (drain it)
//   - a follow-up or steer message arrives on the session's queues
//   - the context is cancelled
//
// Returns true when it consumed something and the outer loop should take
// another turn.
func (sr *sessionRunner) waitForSubagentInbox(ctx context.Context, sess *session.Session, events chan Event) bool {
	if sr.drainSubagentInbox(sess, events) {
		return true
	}
	if !sr.core.subagents.HasInFlightChildren(sess.ID) {
		return false
	}

	a := sr.root.resolveSessionAgent(sess)
	events <- ParentIdle(sess.ID, a.Name())
	defer func() { events <- ParentResume(sess.ID, a.Name()) }()

	inboxSignal := sr.core.subagents.ParentInboxSignal(sess.ID)
	followUpSignal := sr.state.followUp.Signal()
	steerSignal := sr.state.steer.Signal()

	for {
		if sr.drainSubagentInbox(sess, events) {
			return true
		}
		if sr.drainParentFollowUp(sess, events) {
			return true
		}
		if sr.drainParentSteer(sess, events) {
			return true
		}
		if !sr.core.subagents.HasInFlightChildren(sess.ID) {
			return false
		}

		select {
		case <-ctx.Done():
			return false
		case <-inboxSignal:
		case <-followUpSignal:
		case <-steerSignal:
		}
	}
}

// drainParentFollowUp pops exactly one follow-up for the parent
// session, injects it, and returns true.
func (sr *sessionRunner) drainParentFollowUp(sess *session.Session, events chan Event) bool {
	fm, ok := sr.state.followUp.Dequeue(context.Background())
	if !ok {
		return false
	}
	injectUserMessage(sess, fm.Content, fm.MultiContent, func(ev Event) { events <- ev })
	return true
}

// drainParentSteer drains pending steer messages for the parent session
// and returns true if anything was delivered.
func (sr *sessionRunner) drainParentSteer(sess *session.Session, events chan Event) bool {
	steered := sr.state.steer.Drain(context.Background())
	if len(steered) == 0 {
		return false
	}
	for _, sm := range steered {
		injectUserMessage(sess, sm.Content, sm.MultiContent, func(ev Event) { events <- ev })
	}
	return true
}

// ── Stream lifecycle ───────────────────────────────────────────────────

// runStreamWithConfig is the unified session-stream entry point shared
// by root sessions ([LocalRuntime.RunStream]) and child sessions
// ([LocalRuntime.StartChildLoop]). It handles the full session lifecycle:
// tracing span, elicitation channel swap, session hooks, initial tool
// loading, StreamStarted/StreamStopped emission, and drives the engine
// with the given policy.
//
// For non-sub-session roots (Session.IsSubSession returns false) it also
// persists initial session metadata via the session store.
func (sr *sessionRunner) runStreamWithConfig(ctx context.Context, cfg sessionRunConfig) <-chan Event {
	sess := cfg.sess
	a := cfg.agent
	if a == nil {
		a = sr.root.resolveSessionAgent(sess)
	}
	slog.Debug("Starting runtime stream", "agent", a.Name(), "session_id", sess.ID)
	events := make(chan Event, 128)
	external := sr.root.wrapEventsForObservers(sess.ID, events)

	go func() {
		if !sess.IsSubSession() {
			if err := sr.core.sessionStore.UpdateSession(ctx, sess); err != nil {
				slog.Warn("Failed to persist initial session", "session_id", sess.ID, "error", err)
			}
		}

		telemetry.RecordSessionStart(ctx, a.Name(), sess.ID)

		ctx, sessionSpan := sr.root.startSpan(ctx, "runtime.session", trace.WithAttributes(
			attribute.String("agent", a.Name()),
			attribute.String("session.id", sess.ID),
		))
		defer sessionSpan.End()

		prevElicitationCh := sr.swapElicitationEventsChannel(events)

		sr.root.executeSessionStartHooks(ctx, sess, a, events)

		events <- TeamInfo(sr.root.agentDetailsFromTeam(), a.Name())

		sr.root.emitAgentWarnings(a, chanSend(events))
		sr.configureToolsetHandlers(a, sess, events)

		agentTools, err := sr.root.getTools(ctx, a, sessionSpan, events)
		if err != nil {
			events <- Error(fmt.Sprintf("failed to get tools: %v", err))
			return
		}
		agentTools = filterExcludedTools(agentTools, sess.ExcludedTools)

		events <- ToolsetInfo(len(agentTools), false, a.Name())

		messages := sess.GetMessages(a)
		if sess.SendUserMessage && len(messages) > 0 {
			lastMsg := messages[len(messages)-1]
			events <- UserMessage(lastMsg.Content, sess.ID, lastMsg.MultiContent, sess.Len()-1)
		}

		events <- StreamStarted(sess.ID, a.Name())

		defer sr.finalizeEventChannel(ctx, sess, prevElicitationCh, events)

		engine := newSessionEngine(sr, sess, cfg.policy)
		engine.run(ctx, sessionSpan, events)
	}()

	return external
}

// finalizeEventChannel performs cleanup at the end of a
// runStreamWithConfig goroutine: restores the previous elicitation
// channel, emits the StreamStopped event, fires session-end hooks, and
// closes the events channel.
func (sr *sessionRunner) finalizeEventChannel(ctx context.Context, sess *session.Session, prevElicitationCh, events chan Event) {
	sr.swapElicitationEventsChannel(prevElicitationCh)

	defer close(events)

	a := sr.root.resolveSessionAgent(sess)

	sr.root.executeSessionEndHooks(context.WithoutCancel(ctx), sess, a)

	events <- StreamStopped(sess.ID, a.Name())

	sr.root.executeOnUserInputHooks(ctx, a, sess.ID, "stream stopped")

	telemetry.RecordSessionEnd(ctx)
}
