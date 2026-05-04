// engine.go contains the core per-session engine shared by root sessions and
// subagent child sessions: run configuration, turn outcomes, and the main
// per-turn execution loop.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/compaction"
	"github.com/docker/docker-agent/pkg/modelerrors"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/telemetry"
	"github.com/docker/docker-agent/pkg/tools/builtin"
	bgagent "github.com/docker/docker-agent/pkg/tools/builtin/agent"
)

// sessionRunConfig bundles everything [sessionRunner.runStreamWithConfig]
// needs to set up and drive a session through the unified [sessionEngine].
// It is internal to the runtime package; the public [Runtime.RunStream]
// signature remains unchanged.
type sessionRunConfig struct {
	sess   *session.Session
	agent  *agent.Agent // optional; if nil, resolved from sess via resolveSessionAgent
	policy wakePolicy
}

// turnOutcome describes how a single per-turn call ended.
type turnOutcome int

const (
	// outcomeContinue means another iteration should follow immediately,
	// either because in-turn drains injected work or because the model has
	// not yet asked to stop.
	outcomeContinue turnOutcome = iota

	// outcomeStopped means the model asked to stop. The caller should
	// consult [wakePolicy.wakeNext] to decide whether to start another
	// turn (e.g. follow-up message, subagent inbox wake) or terminate.
	outcomeStopped

	// outcomeDone means the engine should terminate immediately. The turn
	// body has already emitted whatever final events are appropriate
	// (StreamStopped is emitted by the caller's deferred finalizer).
	outcomeDone
)

// turnInfo carries the per-turn data a wakePolicy needs to make its
// decision and inject session work cleanly.
type turnInfo struct {
	sess                    *session.Session
	agent                   *agent.Agent
	contextLimit            int64
	messageCountBeforeTools int
	responseContent         string
	canceled                bool // true when the per-turn ctx was canceled but the engine ctx is alive
}

// sessionEngine drives a single live session through the unified outer
// loop. Each engine instance is single-use: build it, call run, discard.
//
// The engine reaches per-session execution dependencies through its
// [sessionRunner] (runner.state for coordination channels, runner.toolMap
// for tool dispatch, runner.core for shared services, runner.root for
// runtime methods that have not yet been session-localised). Root
// sessions get a runner whose state is shared with the LocalRuntime that
// exposes the public Resume / Steer / FollowUp / ResumeElicitation APIs;
// child sessions get a runner with a freshly-allocated state so concurrent
// child loops do not collide on the parent's coordination channels.
type sessionEngine struct {
	runner *sessionRunner
	sess   *session.Session
	policy wakePolicy

	// Per-engine iteration state. These were previously local variables
	// inside the RunStream goroutine; hoisting them onto the engine lets
	// runOneTurn be a method without a sprawling argument list.
	iteration            int
	runtimeMaxIterations int
	overflowCompactions  int
	toolModelOverride    string
	prevAgentName        string
	loopDetector         *toolLoopDetector
}

// newSessionEngine constructs a fresh engine for sess driven by the given
// policy. The engine reads tunables (max iterations, loop detector
// threshold) off the session at construction time so subsequent runs use
// the values the caller passed in.
//
// runner is the engine-owned per-session driver; root callers pass a
// runner built from the root *LocalRuntime, child callers pass a runner
// built around a freshly allocated state.
func newSessionEngine(runner *sessionRunner, sess *session.Session, policy wakePolicy) *sessionEngine {
	loopThreshold := sess.MaxConsecutiveToolCalls
	if loopThreshold == 0 {
		loopThreshold = 5 // default: always active
	}
	return &sessionEngine{
		runner:               runner,
		sess:                 sess,
		policy:               policy,
		runtimeMaxIterations: sess.MaxIterations,
		loopDetector: newToolLoopDetector(loopThreshold,
			bgagent.ToolNameViewBackgroundAgent,
			builtin.ToolNameViewBackgroundJob,
		),
	}
}

// run drives the outer iteration loop until the policy says we are done
// or the context is cancelled.
//
// run does NOT emit StreamStarted or StreamStopped, manage the
// elicitation channel swap, fire session-start hooks, or close the events
// channel. Those are owned by the caller's setup/teardown code so this
// helper stays focused on per-turn orchestration.
func (e *sessionEngine) run(ctx context.Context, sessionSpan trace.Span, events chan Event) {
	// Register this session with the live-session registry so that
	// [LocalRuntime.LiveSessionTree] sees the actual session metadata
	// (agent identity, created_at) rather than synthesising it from the
	// runtime's mutable global state.
	if e.runner.core.liveSessions != nil {
		kind := LiveSessionRoot
		if e.sess.IsSubSession() {
			kind = LiveSessionSubAgent
		}
		e.runner.core.liveSessions.register(e.sess, e.runner.root.resolveSessionAgent(e.sess).Name(), kind)
		defer e.runner.core.liveSessions.unregister(e.sess.ID)
	}

	for {
		turnCtx, cancelTurn := e.policy.turnCtx(ctx)
		turnAgent := e.runner.root.resolveSessionAgent(e.sess)
		events <- TurnStarted(e.sess.ID, turnAgent.Name())
		outcome, info, err := e.runOneTurn(turnCtx, sessionSpan, events)
		events <- TurnEnded(e.sess.ID, turnAgent.Name())
		cancelTurn()

		// Distinguish per-turn cancel (user interrupt) from outer ctx cancel.
		// If the outer ctx is still alive but the turn exited because its own
		// derived ctx was canceled, convert to outcomeStopped so the policy
		// can handle it as a "soft stop" (e.g. MarkWaitingSilently for child
		// sessions).
		if outcome == outcomeDone && err != nil && errors.Is(err, context.Canceled) && ctx.Err() == nil {
			a := e.runner.root.resolveSessionAgent(e.sess)
			info = turnInfo{
				sess:     e.sess,
				agent:    a,
				canceled: true,
			}
			outcome = outcomeStopped
		}

		switch outcome {
		case outcomeContinue:
			continue
		case outcomeStopped:
			if e.policy.wakeNext(ctx, e, info, events) {
				continue
			}
			return
		case outcomeDone:
			return
		}
	}
}

// runOneTurn runs a single iteration of the per-turn body. It returns:
//
//   - outcomeContinue when in-turn drains injected work or the model
//     wants more turns (no stop signal observed);
//   - outcomeStopped after the model emitted a stopped assistant turn
//     and no in-turn drains fired - the caller should consult the
//     wakePolicy;
//   - outcomeDone when the engine should terminate immediately (fatal
//     error, max iterations rejected, context cancelled).
//
// runOneTurn is intentionally large: it owns the iteration-limit gate,
// model selection, fallback chain, tool dispatch, in-turn steer and
// subagent-envelope drains, and per-iteration token-usage emission.
func (e *sessionEngine) runOneTurn(ctx context.Context, sessionSpan trace.Span, events chan Event) (turnOutcome, turnInfo, error) {
	r := e.runner.root
	sess := e.sess

	a := r.resolveSessionAgent(sess)

	// Clear per-tool model override on agent switch so it doesn't leak
	// from one agent's toolset into another agent's turn.
	if a.Name() != e.prevAgentName {
		e.toolModelOverride = ""
		e.prevAgentName = a.Name()
	}

	r.emitAgentWarnings(a, chanSend(events))
	e.runner.configureToolsetHandlers(a, sess, events)

	agentTools, err := r.getTools(ctx, a, sessionSpan, events)
	if err != nil {
		events <- Error(fmt.Sprintf("failed to get tools: %v", err))
		return outcomeDone, turnInfo{}, err
	}
	agentTools = filterExcludedTools(agentTools, sess.ExcludedTools)

	// Emit updated tool count. After a ToolListChanged MCP notification
	// the cache is invalidated, so getTools above re-fetches from the
	// server and may return a different count.
	events <- ToolsetInfo(len(agentTools), false, a.Name())

	// Check iteration limit
	if e.runtimeMaxIterations > 0 && e.iteration >= e.runtimeMaxIterations {
		slog.Debug(
			"Maximum iterations reached",
			"agent", a.Name(),
			"iterations", e.iteration,
			"max", e.runtimeMaxIterations,
		)

		events <- MaxIterationsReached(e.runtimeMaxIterations)

		maxIterMsg := fmt.Sprintf("Maximum iterations reached (%d)", e.runtimeMaxIterations)
		r.executeNotificationHooks(ctx, a, sess.ID, "warning", maxIterMsg)
		r.executeOnUserInputHooks(ctx, a, sess.ID, "max iterations reached")

		// In non-interactive mode (e.g. MCP server), auto-stop instead of
		// blocking forever waiting for user input.
		if sess.NonInteractive {
			slog.Debug("Auto-stopping after max iterations (non-interactive)", "agent", a.Name())

			assistantMessage := chat.Message{
				Role: chat.MessageRoleAssistant,
				Content: fmt.Sprintf(
					"Execution stopped after reaching the configured max_iterations limit (%d).",
					e.runtimeMaxIterations,
				),
				CreatedAt: time.Now().Format(time.RFC3339),
			}

			addAgentMessage(sess, a, &assistantMessage, events)
			return outcomeDone, turnInfo{}, nil
		}

		// Wait for user decision (resume / reject)
		select {
		case req := <-e.runner.state.resumeChan:
			if req.Type == ResumeTypeApprove {
				slog.Debug("User chose to continue after max iterations", "agent", a.Name())
				e.runtimeMaxIterations = e.iteration + 10
			} else {
				slog.Debug("User rejected continuation", "agent", a.Name())

				assistantMessage := chat.Message{
					Role: chat.MessageRoleAssistant,
					Content: fmt.Sprintf(
						"Execution stopped after reaching the configured max_iterations limit (%d).",
						e.runtimeMaxIterations,
					),
					CreatedAt: time.Now().Format(time.RFC3339),
				}

				addAgentMessage(sess, a, &assistantMessage, events)
				return outcomeDone, turnInfo{}, nil
			}

		case <-ctx.Done():
			slog.Debug(
				"Context cancelled while waiting for resume confirmation",
				"agent", a.Name(),
				"session_id", sess.ID,
			)
			return outcomeDone, turnInfo{}, ctx.Err()
		}
	}

	e.iteration++

	// Exit immediately if the stream context has been cancelled (e.g., Ctrl+C)
	if err := ctx.Err(); err != nil {
		slog.Debug("Runtime stream context cancelled, stopping loop", "agent", a.Name(), "session_id", sess.ID)
		return outcomeDone, turnInfo{}, err
	}
	slog.Debug("Starting conversation loop iteration", "agent", a.Name())

	streamCtx, streamSpan := r.startSpan(ctx, "runtime.stream", trace.WithAttributes(
		attribute.String("agent", a.Name()),
		attribute.String("session.id", sess.ID),
	))

	model := a.Model()

	// Per-tool model routing: use a cheaper model for this turn if the
	// previous tool calls specified one, then reset.
	if e.toolModelOverride != "" {
		if overrideModel, err := r.resolveModelRef(ctx, e.toolModelOverride); err != nil {
			slog.Warn("Failed to resolve per-tool model override; using agent default",
				"model_override", e.toolModelOverride, "error", err)
		} else {
			slog.Info("Using per-tool model override for this turn",
				"agent", a.Name(), "override", overrideModel.ID(), "primary", model.ID())
			model = overrideModel
		}
		e.toolModelOverride = ""
	}

	modelID := model.ID()

	// Notify sidebar of the model for this turn. For rule-based routing,
	// the actual routed model is emitted from within the stream once the
	// first chunk arrives.
	events <- AgentInfo(a.Name(), modelID, a.Description(), a.WelcomeMessage())

	slog.Debug("Using agent", "agent", a.Name(), "model", modelID)
	slog.Debug("Getting model definition", "model_id", modelID)
	m, err := r.modelsStore.GetModel(ctx, modelID)
	if err != nil {
		slog.Debug("Failed to get model definition", "error", err)
	}

	// We can only compact if we know the limit.
	var contextLimit int64
	if m != nil {
		contextLimit = int64(m.Limit.Context)

		if r.sessionCompaction && compaction.ShouldCompact(sess.InputTokens, sess.OutputTokens, 0, contextLimit) {
			r.Summarize(ctx, sess, "", events)
		}
	}

	messages := sess.GetMessages(a)
	slog.Debug("Retrieved messages for processing", "agent", a.Name(), "message_count", len(messages))

	// Strip image content from messages if the model doesn't support
	// image input. This prevents API errors when conversation history
	// contains images (e.g. from tool results or user attachments) but
	// the current model is text-only.
	if m != nil && len(m.Modalities.Input) > 0 && !slices.Contains(m.Modalities.Input, "image") {
		messages = stripImageContent(messages)
	}

	// Try primary model with fallback chain if configured
	res, usedModel, err := r.tryModelWithFallback(streamCtx, a, model, messages, agentTools, sess, m, events)
	if err != nil {
		// Treat stream-context cancellation/deadline as a graceful stop.
		// This covers both explicit user interrupts (context.Canceled)
		// and externally-imposed deadlines (context.DeadlineExceeded)
		// without misclassifying them as model/provider failures.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			slog.Debug("Model stream ended by context", "agent", a.Name(), "session_id", sess.ID, "error", err)
			streamSpan.End()
			return outcomeDone, turnInfo{}, err
		}

		// Auto-recovery: if the error is a context overflow and session
		// compaction is enabled, compact the conversation and retry the
		// request instead of surfacing raw errors. We allow at most
		// maxOverflowCompactions consecutive attempts to avoid an
		// infinite loop when compaction cannot reduce the context
		// enough.
		const maxOverflowCompactions = 1
		if _, ok := errors.AsType[*modelerrors.ContextOverflowError](err); ok && r.sessionCompaction && e.overflowCompactions < maxOverflowCompactions {
			e.overflowCompactions++
			slog.Warn("Context window overflow detected, attempting auto-compaction",
				"agent", a.Name(),
				"session_id", sess.ID,
				"input_tokens", sess.InputTokens,
				"output_tokens", sess.OutputTokens,
				"context_limit", contextLimit,
				"attempt", e.overflowCompactions,
			)
			events <- Warning(
				"The conversation has exceeded the model's context window. Automatically compacting the conversation history...",
				a.Name(),
			)
			r.Summarize(ctx, sess, "", events)

			// After compaction, loop back to retry with the compacted
			// context. The next iteration will re-fetch messages from
			// the (now compacted) session.
			streamSpan.End()
			return outcomeContinue, turnInfo{}, nil
		}

		streamSpan.RecordError(err)
		streamSpan.SetStatus(codes.Error, "error handling stream")
		slog.Error("All models failed", "agent", a.Name(), "error", err)
		// Track error in telemetry
		telemetry.RecordError(ctx, err.Error())
		errMsg := modelerrors.FormatError(err)
		events <- Error(errMsg)
		r.executeNotificationHooks(ctx, a, sess.ID, "error", errMsg)
		streamSpan.End()
		return outcomeDone, turnInfo{}, err
	}

	// A successful model call resets the overflow compaction counter.
	e.overflowCompactions = 0

	if usedModel != nil && usedModel.ID() != model.ID() {
		slog.Info("Used fallback model", "agent", a.Name(), "primary", model.ID(), "used", usedModel.ID())
		events <- AgentInfo(a.Name(), usedModel.ID(), a.Description(), a.WelcomeMessage())
	}
	streamSpan.SetAttributes(
		attribute.Int("tool.calls", len(res.Calls)),
		attribute.Int("content.length", len(res.Content)),
		attribute.Bool("stopped", res.Stopped),
	)
	streamSpan.End()
	slog.Debug("Stream processed", "agent", a.Name(), "tool_calls", len(res.Calls), "content_length", len(res.Content), "stopped", res.Stopped)

	msgUsage := r.recordAssistantMessage(sess, a, res, agentTools, modelID, m, events)

	usage := SessionUsage(sess, contextLimit)
	usage.LastMessage = msgUsage
	events <- NewTokenUsageEvent(sess.ID, a.Name(), usage)

	// Record the message count before tool calls so we can measure how
	// much content was added by tool results.
	messageCountBeforeTools := len(sess.GetAllMessages())

	e.runner.processToolCalls(ctx, sess, res.Calls, agentTools, events)

	// Check for degenerate tool call loops
	if e.loopDetector.record(res.Calls) {
		toolName := "unknown"
		if len(res.Calls) > 0 {
			toolName = res.Calls[0].Function.Name
		}
		slog.Warn("Repetitive tool call loop detected",
			"agent", a.Name(), "tool", toolName,
			"consecutive", e.loopDetector.consecutive, "session_id", sess.ID)
		errMsg := fmt.Sprintf(
			"Agent terminated: detected %d consecutive identical calls to %s. "+
				"This indicates a degenerate loop where the model is not making progress.",
			e.loopDetector.consecutive, toolName)
		events <- Error(errMsg)
		r.executeNotificationHooks(ctx, a, sess.ID, "error", errMsg)
		e.loopDetector.reset()
		return outcomeDone, turnInfo{}, nil
	}

	// Record per-toolset model override for the next LLM turn.
	e.toolModelOverride = resolveToolCallModelOverride(res.Calls, agentTools)

	// --- STEERING: mid-turn injection ---
	// Drain ALL pending steer messages. These are urgent course-
	// corrections that the model should see on the very next iteration.
	if steered := e.runner.state.steer.Drain(ctx); len(steered) > 0 {
		for _, sm := range steered {
			injectUserMessage(sess, sm.Content, sm.MultiContent, func(ev Event) { events <- ev })
		}

		r.compactIfNeeded(ctx, sess, a, m, contextLimit, messageCountBeforeTools, events)
		return outcomeContinue, turnInfo{}, nil
	}

	// Deliver any pending subagent updates at the same safe point as
	// steer messages. This preserves chat ordering and guarantees the
	// parent never sees child updates interleaved into an unsafe part of
	// its own streamed turn.
	if e.runner.drainSubagentInbox(sess, events) {
		r.compactIfNeeded(ctx, sess, a, m, contextLimit, messageCountBeforeTools, events)
		return outcomeContinue, turnInfo{}, nil
	}

	// Let the wake policy inject policy-specific mid-turn messages.
	// For child sessions this drains the direct parent→child inbox so
	// steer-mode messages can land during a running turn; root sessions
	// return false (no-op).
	if e.policy.drainMidTurn(sess, events) {
		r.compactIfNeeded(ctx, sess, a, m, contextLimit, messageCountBeforeTools, events)
		return outcomeContinue, turnInfo{}, nil
	}

	if res.Stopped {
		return outcomeStopped, turnInfo{
			sess:                    sess,
			agent:                   a,
			contextLimit:            contextLimit,
			messageCountBeforeTools: messageCountBeforeTools,
			responseContent:         res.Content,
		}, nil
	}

	r.compactIfNeeded(ctx, sess, a, m, contextLimit, messageCountBeforeTools, events)
	return outcomeContinue, turnInfo{}, nil
}
