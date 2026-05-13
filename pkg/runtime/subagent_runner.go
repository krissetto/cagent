package runtime

import (
	"context"
	"log/slog"
	"strings"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

// Compile-time check that LocalRuntime implements subagent.Runner.
var _ subagent.Runner = (*LocalRuntime)(nil)

// StartChildLoop implements subagent.Runner. It creates an isolated
// sessionState for the child, and drives the subagent's keep-alive loop
// through the same runStreamLoop + wakePolicy pattern used by root sessions.
func (r *LocalRuntime) StartChildLoop(ctx context.Context, h *subagent.Handle) <-chan struct{} {
	done := make(chan struct{})

	childState := &sessionState{
		steerQueue:           NewInMemoryMessageQueue(defaultSteerQueueCapacity),
		followUpQueue:        NewInMemoryMessageQueue(defaultFollowUpQueueCapacity),
		resumeChan:           make(chan ResumeRequest),
		elicitationRequestCh: make(chan ElicitationResult),
		elicitation:          &elicitationBridge{},
		isChild:              true,
	}

	go func() {
		defer close(done)
		defer r.subagents.CascadeStop(h.ID())
		defer func() {
			if r.recorder != nil {
				r.recorder.FlushSession(h.ID())
			}
			if r.eventBus != nil {
				r.eventBus.CloseTopic(h.ID())
			}
		}()

		h.MarkRunning()

		// Kick off title generation early.
		if sess := h.Session(); sess != nil && strings.TrimSpace(sess.Title) == "" {
			go r.generateSubagentTitle(ctx, h.CloseCh(), sess, r.resolveSessionAgent(sess))
		}

		// Check for early close/cancel before kicking off the loop.
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
			r:             r,
			h:             h,
			childInboxSig: r.subagents.ParentInboxSignal(h.ID()),
		}

		// Set the wake policy on the child state so the loop can use it.
		childState.wakePolicy = policy

		// Drive the subagent through the standard runStreamLoop.
		events := make(chan Event, defaultEventChannelCapacity)
		go r.runStreamLoop(ctx, childState, h.Session(), events)

		// Drain the events channel, collecting any error event.
		var lastErr string
		for ev := range events {
			if errEv, ok := ev.(*ErrorEvent); ok && lastErr == "" {
				lastErr = errEv.Error
			}
			// Publish events to the bus for live observers.
			if r.eventBus != nil {
				r.eventBus.Publish(h.ID(), ev)
			}
		}

		publishTerminalEnvelope(ctx, h, lastErr)
	}()

	return done
}

// newSubagentChildSession builds a fresh child session pinned to the given agent.
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
		session.WithNonInteractive(true),
	}

	excluded := mergeExcludedTools(parent.ExcludedTools, cfg.ExcludedTools)
	if len(excluded) > 0 {
		opts = append(opts, session.WithExcludedTools(excluded))
	}
	return session.New(opts...)
}

// buildSubagentInitialUserMessage renders the delegated task as a plain
// user message.
func buildSubagentInitialUserMessage(cfg subagent.StartConfig) string {
	if text := strings.TrimSpace(cfg.InitialMessage.Content); text != "" {
		return text
	}
	if task := strings.TrimSpace(cfg.Task); task != "" {
		return task
	}
	return "Please proceed."
}

// generateSubagentTitle asks the child agent's own model to generate a
// session title, mirroring the normal user-initiated title flow.
func (r *LocalRuntime) generateSubagentTitle(ctx context.Context, stop <-chan struct{}, sess *session.Session, childAgent *agent.Agent) {
	if sess == nil || childAgent == nil || strings.TrimSpace(sess.Title) != "" {
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

	gen := r.TitleGenerator()
	if gen == nil {
		return
	}

	// Collect recent user messages for title generation.
	all := sess.GetAllMessages()
	var userMsgs []string
	for _, msg := range all {
		if msg.Message.Role == "user" {
			content := strings.TrimSpace(msg.Message.Content)
			if content != "" {
				userMsgs = append(userMsgs, content)
			}
		}
	}
	if len(userMsgs) > 2 {
		userMsgs = userMsgs[len(userMsgs)-2:]
	}
	if len(userMsgs) == 0 {
		return
	}

	title, err := gen.Generate(ctx, sess.ID, userMsgs)
	if err != nil {
		slog.DebugContext(ctx, "Failed to generate subagent title", "session_id", sess.ID, "agent", childAgent.Name(), "error", err)
		return
	}
	title = strings.TrimSpace(title)
	if title == "" {
		return
	}

	if err := r.UpdateSessionTitle(ctx, sess, title); err != nil {
		slog.DebugContext(ctx, "Failed to persist subagent title", "session_id", sess.ID, "title", title, "error", err)
	}
	if r.subagents != nil {
		if err := r.subagents.SetTitle(sess.ID, title); err != nil {
			slog.DebugContext(ctx, "Failed to mirror subagent title", "session_id", sess.ID, "title", title, "error", err)
		}
	}
	r.publishSessionEvent(sess.ID, SessionTitle(sess.ID, title))
}

// publishTerminalEnvelope emits the final terminal envelope for a child loop.
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
