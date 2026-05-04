// runtime_stream.go contains the public RunStream/Run entry points plus
// the per-session helpers (tool registration, agent warnings, fan-out)
// that support them. Session-lifecycle setup/teardown — elicitation
// channel swap, hook execution wrapping the engine, and final
// StreamStopped emission — lives on [sessionRunner] (see session_runner.go)
// so root and child sessions share a single implementation path.
package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/compaction"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/telemetry"
	"github.com/docker/docker-agent/pkg/tools"
)

// RunStream starts the agent's interaction loop and returns a channel of events.
// The returned channel is closed when the loop terminates (success, error, or
// context cancellation). Each iteration: sends messages to the model, streams
// the response, executes any tool calls, and loops until the model signals stop
// or the iteration limit is reached.
//
// The per-turn body and the post-stop wake policy live in [sessionEngine] so
// the same shape can drive both root and subagent sessions; RunStream is the
// root-session entry point that builds a root [sessionRunner], an internal
// [sessionRunConfig] with a [rootWakePolicy], and delegates to
// [sessionRunner.runStreamWithConfig].
func (r *LocalRuntime) RunStream(ctx context.Context, sess *session.Session) <-chan Event {
	runner := newRootSessionRunner(r)
	cfg := sessionRunConfig{
		sess:   sess,
		policy: rootWakePolicy{runner: runner},
	}
	return runner.runStreamWithConfig(ctx, cfg)
}

// wrapEventsForObservers spins up a fan-out goroutine that publishes every
// event emitted on the internal channel to the runtime's event bus for the
// given session id and also forwards it to the returned external channel.
// When the internal channel is closed, the goroutine calls
// [SessionRecorder.FlushSession] (if a recorder is configured) to block
// until all in-flight async store writes have been committed. Only then is
// the external channel closed, so consumers that drain the external channel
// to completion can safely read from the session store without racing async
// persistence.
func (r *LocalRuntime) wrapEventsForObservers(sessionID string, internal chan Event) chan Event {
	external := make(chan Event, 128)
	go func() {
		defer close(external)
		for ev := range internal {
			r.publishSessionEvent(sessionID, ev)
			external <- ev
		}
		// Flush the recorder so that all persisted events are committed before
		// external closes. Callers that drain the external channel to completion
		// (e.g. tests, ACP endpoint) can then read from the session store
		// without a race.
		if r.recorder != nil {
			r.recorder.FlushSession(sessionID)
		}
	}()
	return external
}

// Run is a convenience wrapper around RunStream for non-streaming callers.
func (r *LocalRuntime) Run(ctx context.Context, sess *session.Session) ([]session.Message, error) {
	events := r.RunStream(ctx, sess)
	for event := range events {
		if errEvent, ok := event.(*ErrorEvent); ok {
			return nil, fmt.Errorf("%s", errEvent.Error)
		}
	}
	return sess.GetAllMessages(), nil
}

// recordAssistantMessage adds the model's response to the session and returns
// per-message usage information for the token-usage event. Empty responses
// (no text and no tool calls) are silently skipped since providers reject them.
func (r *LocalRuntime) recordAssistantMessage(
	sess *session.Session,
	a *agent.Agent,
	res streamResult,
	agentTools []tools.Tool,
	modelID string,
	m *modelsdev.Model,
	events chan Event,
) *MessageUsage {
	if strings.TrimSpace(res.Content) == "" && len(res.Calls) == 0 {
		slog.Debug("Skipping empty assistant message (no content and no tool calls)", "agent", a.Name())
		return nil
	}

	// Resolve tool definitions for the tool calls.
	var toolDefs []tools.Tool
	if len(res.Calls) > 0 {
		toolMap := make(map[string]tools.Tool, len(agentTools))
		for _, t := range agentTools {
			toolMap[t.Name] = t
		}
		for _, call := range res.Calls {
			if def, ok := toolMap[call.Function.Name]; ok {
				toolDefs = append(toolDefs, def)
			}
		}
	}

	// Calculate per-message cost when pricing information is available.
	var messageCost float64
	if res.Usage != nil && m != nil && m.Cost != nil {
		messageCost = (float64(res.Usage.InputTokens)*m.Cost.Input +
			float64(res.Usage.OutputTokens)*m.Cost.Output +
			float64(res.Usage.CachedInputTokens)*m.Cost.CacheRead +
			float64(res.Usage.CacheWriteTokens)*m.Cost.CacheWrite) / 1e6
	}

	messageModel := modelID

	assistantMessage := chat.Message{
		Role:              chat.MessageRoleAssistant,
		Content:           res.Content,
		ReasoningContent:  res.ReasoningContent,
		ThinkingSignature: res.ThinkingSignature,
		ThoughtSignature:  res.ThoughtSignature,
		ToolCalls:         res.Calls,
		ToolDefinitions:   toolDefs,
		CreatedAt:         time.Now().Format(time.RFC3339),
		Usage:             res.Usage,
		Model:             messageModel,
		Cost:              messageCost,
		FinishReason:      res.FinishReason,
	}

	addAgentMessage(sess, a, &assistantMessage, events)
	slog.Debug("Added assistant message to session", "agent", a.Name(), "total_messages", len(sess.GetAllMessages()))

	// Build per-message usage for the event.
	if res.Usage == nil {
		return nil
	}
	msgUsage := &MessageUsage{
		Usage:        *res.Usage,
		Cost:         messageCost,
		Model:        messageModel,
		FinishReason: res.FinishReason,
	}
	return msgUsage
}

// compactIfNeeded estimates the token impact of tool results added since
// messageCountBefore and triggers proactive compaction when the estimated
// total exceeds 90% of the context window. This prevents sending an
// oversized request on the next iteration.
func (r *LocalRuntime) compactIfNeeded(
	ctx context.Context,
	sess *session.Session,
	a *agent.Agent,
	m *modelsdev.Model,
	contextLimit int64,
	messageCountBefore int,
	events chan Event,
) {
	if m == nil || !r.sessionCompaction || contextLimit <= 0 {
		return
	}

	newMessages := sess.GetAllMessages()[messageCountBefore:]
	var addedTokens int64
	for _, msg := range newMessages {
		addedTokens += compaction.EstimateMessageTokens(&msg.Message)
	}

	if !compaction.ShouldCompact(sess.InputTokens, sess.OutputTokens, addedTokens, contextLimit) {
		return
	}

	slog.Info("Proactive compaction: tool results pushed estimated context past 90%% threshold",
		"agent", a.Name(),
		"input_tokens", sess.InputTokens,
		"output_tokens", sess.OutputTokens,
		"added_estimated_tokens", addedTokens,
		"estimated_total", sess.InputTokens+sess.OutputTokens+addedTokens,
		"context_limit", contextLimit,
	)
	r.Summarize(ctx, sess, "", events)
}

// getTools executes tool retrieval with automatic OAuth handling
func (r *LocalRuntime) getTools(ctx context.Context, a *agent.Agent, sessionSpan trace.Span, events chan Event) ([]tools.Tool, error) {
	shouldEmitMCPInit := len(a.ToolSets()) > 0
	if shouldEmitMCPInit {
		events <- MCPInitStarted(a.Name())
	}
	defer func() {
		if shouldEmitMCPInit {
			events <- MCPInitFinished(a.Name())
		}
	}()

	agentTools, err := a.Tools(ctx)
	if err != nil {
		slog.Error("Failed to get agent tools", "agent", a.Name(), "error", err)
		sessionSpan.RecordError(err)
		sessionSpan.SetStatus(codes.Error, "failed to get tools")
		telemetry.RecordError(ctx, err.Error())
		return nil, err
	}

	slog.Debug("Retrieved agent tools", "agent", a.Name(), "tool_count", len(agentTools))
	return agentTools, nil
}

// emitAgentWarnings drains and emits any agent initialization warnings.
func (r *LocalRuntime) emitAgentWarnings(a *agent.Agent, send func(Event)) {
	warnings := a.DrainWarnings()
	if len(warnings) == 0 {
		return
	}

	slog.Warn("Tool setup partially failed; continuing", "agent", a.Name(), "warnings", warnings)
	send(Warning(formatToolWarning(a, warnings), a.Name()))
}

func formatToolWarning(a *agent.Agent, warnings []string) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "Some toolsets failed to initialize for agent '%s'.\n\nDetails:\n\n", a.Name())
	for _, warning := range warnings {
		fmt.Fprintf(&builder, "- %s\n", warning)
	}
	return strings.TrimSuffix(builder.String(), "\n")
}

// filterExcludedTools removes tools whose names appear in the excluded list.
// This is used by skill sub-sessions to prevent recursive run_skill calls.
func filterExcludedTools(agentTools []tools.Tool, excluded []string) []tools.Tool {
	if len(excluded) == 0 {
		return agentTools
	}
	excludeSet := make(map[string]bool, len(excluded))
	for _, name := range excluded {
		excludeSet[name] = true
	}
	filtered := make([]tools.Tool, 0, len(agentTools))
	for _, t := range agentTools {
		if !excludeSet[t.Name] {
			filtered = append(filtered, t)
		}
	}
	return filtered
}

// chanSend wraps a channel as a func(Event) for use with emitAgentWarnings
// and RAG event forwarding. The send is non-blocking: if the channel is full
// or closed, the event is silently dropped. This prevents a panic when a
// long-lived goroutine (e.g. RAG file watcher) tries to forward an event
// after the per-message events channel has been closed.
func chanSend(ch chan Event) func(Event) {
	return func(e Event) {
		defer func() { recover() }() //nolint:errcheck // swallow send-on-closed-channel panic
		select {
		case ch <- e:
		default:
		}
	}
}
