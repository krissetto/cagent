// subagent_runner.go contains the local in-process implementation of the
// subagent.Runner contract: the child-loop driver, child-session
// construction helpers, and terminal-envelope classification. Per-session
// execution dependencies are owned by [sessionRunner]; this file builds
// child runners through [newChildSessionRunner] rather than constructing
// a child LocalRuntime.
package runtime

import (
	"context"
	"log/slog"
	"strings"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/sessiontitle"
	"github.com/docker/docker-agent/pkg/subagent"
)

func collectRecentUserMessages(sess *session.Session, limit int) []string {
	if sess == nil {
		return nil
	}
	all := sess.GetAllMessages()
	msgs := make([]string, 0, 2)
	for _, msg := range all {
		if msg.Message.Role != chat.MessageRoleUser {
			continue
		}
		content := strings.TrimSpace(msg.Message.Content)
		if content == "" {
			continue
		}
		msgs = append(msgs, content)
	}
	if limit > 0 && len(msgs) > limit {
		return msgs[len(msgs)-limit:]
	}
	return msgs
}

// Ensure runtime satisfies subagent.Runner at compile time.
var _ subagent.Runner = (*LocalRuntime)(nil)

// generateSubagentTitle asks the child agent's own model to generate the
// session title, mirroring the normal user-initiated title flow. On success it
// persists the title into the child session, mirrors it into the live subagent
// handle snapshot, and publishes a SessionTitle event on the child session's
// live event bus so attached tabs update immediately.
//
// stop is the subagent's lifecycle signal. When it fires, title generation is
// canceled so the goroutine does not outlive the child loop and attempt model
// I/O or session mutation after the subagent is already closing/stopped.
//
// Failures are intentionally non-fatal: the subagent's work continues and the
// title is simply left empty so the UI can fall back to agent-name labeling.
// This mirrors the root-session path in [pkg/app.App.generateTitle], which
// also emits an empty SessionTitle event on failure rather than inventing a
// generic label.
func (r *LocalRuntime) generateSubagentTitle(ctx context.Context, stop <-chan struct{}, sess *session.Session, childAgent *agent.Agent) {
	if sess == nil || childAgent == nil || strings.TrimSpace(sess.GetTitle()) != "" {
		return
	}
	if stop != nil {
		childCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		go func() {
			select {
			case <-stop:
				cancel()
			case <-childCtx.Done():
			}
		}()
		ctx = childCtx
	}
	userMessages := collectRecentUserMessages(sess, 2)
	if len(userMessages) == 0 {
		r.publishSessionEvent(sess.ID, SessionTitle(sess.ID, ""))
		return
	}
	gen := sessiontitle.New(childAgent.Model(), childAgent.FallbackModels()...)
	title, err := gen.Generate(ctx, sess.ID, userMessages)
	if err != nil {
		slog.Debug("Failed to generate subagent title", "session_id", sess.ID, "agent", childAgent.Name(), "error", err)
		r.publishSessionEvent(sess.ID, SessionTitle(sess.ID, ""))
		return
	}
	title = strings.TrimSpace(title)
	if title == "" {
		r.publishSessionEvent(sess.ID, SessionTitle(sess.ID, ""))
		return
	}
	if err := r.UpdateSessionTitle(ctx, sess, title); err != nil {
		slog.Debug("Failed to persist subagent title", "session_id", sess.ID, "title", title, "error", err)
		// UpdateSessionTitle already updates sess.Title in-memory before attempting
		// to persist; no need to re-set it here.
	}
	// Mirror the title onto the live handle snapshot so the sidebar, live tree,
	// and any attached-tab seed (via LiveSessionNode.Title) see the generated
	// title even if they were opened after this goroutine completed.
	if r.subagents != nil {
		if err := r.subagents.SetTitle(sess.ID, title); err != nil {
			slog.Debug("Failed to mirror subagent title into handle snapshot", "session_id", sess.ID, "title", title, "error", err)
		}
	}
	r.publishSessionEvent(sess.ID, SessionTitle(sess.ID, title))
}

// StartChildLoop implements subagent.Runner. It creates an isolated child
// [sessionRunner] and drives the subagent's keep-alive loop through the
// same [sessionEngine] + [wakePolicy] pattern used by root sessions, with
// a [childWakePolicy] that handles parent→child messages, descendant
// envelope wake-ups, and close/interrupt signals.
//
// The engine runs once for the full subagent lifetime. Per-turn observability
// is provided by [TurnStartedEvent] / [TurnEndedEvent] emitted from inside
// [sessionEngine.run]; [StreamStartedEvent] / [StreamStoppedEvent] mark the
// full child session lifetime and are emitted exactly once by
// [sessionRunner.runStreamWithConfig] and [sessionRunner.finalizeEventChannel].
func (r *LocalRuntime) StartChildLoop(ctx context.Context, h *subagent.Handle) <-chan struct{} {
	done := make(chan struct{})
	// Allocate the child's per-session state explicitly so it is obvious that
	// the child shares no coordination channels with its parent.
	childState := newSessionState(h.AgentName())
	childRunner := newChildSessionRunner(r, childState)

	go func() {
		defer close(done)
		defer r.eventBus.CloseTopic(h.ID())
		// Stop the child runner's background-agent tasks before closing the
		// event-bus topic, so any in-flight bgAgent goroutines can still emit
		// events while they wind down. The root LocalRuntime.Close only stops
		// the root bgAgents handler; child runners own their own handler
		// (newChildSessionRunner) and must be drained here, otherwise
		// background tasks launched from child sessions via
		// run_background_agent would outlive the subagent loop.
		defer childRunner.bgAgents.StopAll()
		defer r.subagents.CascadeStop(h.ID())

		h.MarkRunning()

		// Kick off title generation as early as possible.
		if sess := h.Session(); sess != nil && strings.TrimSpace(sess.GetTitle()) == "" {
			go r.generateSubagentTitle(ctx, h.CloseCh(), sess, r.resolveSessionAgent(sess))
		}

		// Check for early close/cancel before kicking off the engine.
		select {
		case <-h.CloseCh():
			h.PublishClosed()
			return
		case <-ctx.Done():
			h.PublishStopped()
			return
		default:
		}

		policy := &childWakePolicy{
			runner:         childRunner,
			h:              h,
			childInboxSig:  r.subagents.ParentInboxSignal(h.ID()),
			directInboxSig: h.InboxSignal(),
			steerInboxSig:  h.SteerInboxSignal(),
		}

		// Drive the full subagent lifetime through the unified engine.
		// runStreamWithConfig handles StreamStarted/Stopped, hooks, tool
		// loading, and the per-turn engine loop.
		cfg := sessionRunConfig{
			sess:   h.Session(),
			policy: policy,
		}
		stream := childRunner.runStreamWithConfig(ctx, cfg)

		// Drain the events channel.
		var lastErr string
		for ev := range stream {
			if errEv, ok := ev.(*ErrorEvent); ok && lastErr == "" {
				lastErr = errEv.Error
			}
		}

		publishTerminalEnvelope(ctx, h, lastErr)
	}()

	return done
}

// newSubagentChildSession builds and wires up a fresh child session
// pinned to the given agent. It attaches the child to the parent once
// complete.
//
// Principle: subagents receive the delegated task as a normal user
// message. The child agent's own system prompt is left 100% untouched,
// so the user's mental model ("the parent just sends the subagent a
// message") matches the actual wire-level behaviour. No synthetic
// "Please proceed." placeholder is injected.
func (r *LocalRuntime) newSubagentChildSession(parent *session.Session, cfg subagent.StartConfig, a *agent.Agent) *session.Session {
	opts := []session.Opt{
		session.WithUserMessage(buildSubagentInitialUserMessage(cfg)),
		session.WithMaxIterations(a.MaxIterations()),
		session.WithMaxConsecutiveToolCalls(a.MaxConsecutiveToolCalls()),
		session.WithMaxOldToolCallTokens(a.MaxOldToolCallTokens()),
		session.WithTitle(cfg.Title),
		session.WithToolsApproved(cfg.ToolsApproved),
		session.WithSendUserMessage(false),
		session.WithParentID(parent.ID),
		session.WithAgentName(cfg.AgentName),
		// Subagents are background workers with no user attached, so the
		// max-iterations gate in the session engine must auto-stop rather
		// than block on resumeChan waiting for an interactive approval
		// path that does not exist for child sessions.
		session.WithNonInteractive(true),
	}

	excluded := mergeExcludedTools(parent.ExcludedTools, cfg.ExcludedTools)
	if len(excluded) > 0 {
		opts = append(opts, session.WithExcludedTools(excluded))
	}
	return session.New(opts...)
}

// buildSubagentInitialUserMessage renders the delegated task as a plain user
// message. The runtime validates Task server-side before reaching this path;
// the empty-task fallback exists only for tests and edge cases.
func buildSubagentInitialUserMessage(cfg subagent.StartConfig) string {
	if text := strings.TrimSpace(cfg.InitialMessage.Content); text != "" {
		return text
	}
	if task := strings.TrimSpace(cfg.Task); task != "" {
		return task
	}
	return "Please proceed."
}

// publishTerminalEnvelope emits the final terminal envelope for a child loop
// after its unified engine stream has fully exited and its events channel has
// been drained.
//
// StartChildLoop owns the subagent goroutine lifetime, so it is the right
// place to classify why the child ended:
//   - canceled engine context => stopped
//   - observed ErrorEvent     => failed
//   - explicit close request  => closed
//   - any other clean exit    => stopped
//
// The final default-to-stopped case preserves the regression fix covered by
// TestSubagent_NaturalExitPublishesTerminalEnvelope: a non-interactive child
// hitting max_iterations exits the engine cleanly without an error event, but
// still must publish a terminal lifecycle update.
func publishTerminalEnvelope(engineCtx context.Context, h *subagent.Handle, lastErr string) {
	switch {
	case engineCtx != nil && engineCtx.Err() != nil:
		h.PublishStopped()
	case lastErr != "":
		h.PublishFailure(lastErr)
	default:
		select {
		case <-h.CloseCh():
			h.PublishClosed()
		default:
			h.PublishStopped()
		}
	}
}
