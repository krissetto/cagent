// File subagent_tools.go contains model-facing subagent tool handlers plus small
// transcript/serialization helpers used by inspect-style tool responses.

package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tools"
)

// resolveSubagentRef resolves a model-facing subagent id (short or full) to
// a full child id scoped to the given parent session.
func (r *LocalRuntime) resolveSubagentRef(sess *session.Session, ref string) (string, subagent.HandleSnapshot, *tools.ToolCallResult) {
	if strings.TrimSpace(ref) == "" {
		return "", subagent.HandleSnapshot{}, tools.ResultError("subagent_id is required")
	}
	id, err := r.subagents.ResolveChildRef(sess.ID, ref)
	if err != nil {
		return "", subagent.HandleSnapshot{}, tools.ResultError(err.Error())
	}
	snap, err := r.subagents.Get(id)
	if err != nil {
		return "", subagent.HandleSnapshot{}, tools.ResultError(err.Error())
	}
	if snap.ParentSessionID != sess.ID {
		return "", subagent.HandleSnapshot{}, tools.ResultError(fmt.Sprintf("subagent %s does not belong to this session", subagent.ShortRef(id)))
	}
	return id, snap, nil
}

// handleSubagentStart is the runtime-side handler for the subagent_start tool.
func (r *LocalRuntime) handleSubagentStart(ctx context.Context, sess *session.Session, toolCall tools.ToolCall, events EventSink) (*tools.ToolCallResult, error) {
	var params subagent.StartArgs
	if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if strings.TrimSpace(params.Agent) == "" {
		return tools.ResultError("agent name must not be empty"), nil
	}
	if strings.TrimSpace(params.Task) == "" {
		return tools.ResultError("task must not be empty"), nil
	}

	parentAgent := r.resolveSessionAgent(sess)
	subs := parentAgent.RuntimeSubagents()
	if errRes := validateAgentInList(parentAgent.Name(), params.Agent, "start subagent", "runtime subagents list", subs); errRes != nil {
		return errRes, nil
	}

	childAgent, err := r.team.Agent(params.Agent)
	if err != nil {
		return nil, err
	}

	cfg := subagent.StartConfig{
		Parent:        sess,
		AgentName:     params.Agent,
		Task:          params.Task,
		ToolsApproved: true,
	}

	childSess := r.newSubagentChildSession(sess, cfg, childAgent)

	// Decouple child lifetime from the parent's tool-handler ctx.
	h, err := r.subagents.StartChild(context.Background(), cfg, childSess)
	if err != nil {
		return tools.ResultError(fmt.Sprintf("failed to start subagent: %v", err)), nil
	}

	// Persist child session immediately so it is visible in history.
	// Skip in-memory stores because they retain the same session pointer;
	// recorder AddMessage calls would otherwise double-write into the
	// live child session slice.
	if r.sessionStore != nil {
		if _, ok := r.sessionStore.(*session.InMemorySessionStore); !ok {
			if err := r.sessionStore.AddSession(ctx, childSess); err != nil {
				slog.WarnContext(ctx, "Failed to persist subagent session", "session_id", childSess.ID, "error", err)
			}
		}
	}

	events.Emit(SubAgentStarted(h.Snapshot(), sess.ID))

	out := map[string]string{
		"subagent_id": subagent.ShortRef(h.ID()),
		"agent":       h.AgentName(),
		"status":      h.Status().String(),
		"message":     "Subagent started. Updates will be delivered automatically when it responds. Use the returned subagent_id (5-character short ref) for follow-ups.",
	}
	return tools.ResultJSON(out), nil
}

// handleSubagentSend enqueues a follow-up message on an existing subagent.
func (r *LocalRuntime) handleSubagentSend(_ context.Context, sess *session.Session, toolCall tools.ToolCall, events EventSink) (*tools.ToolCallResult, error) {
	var params subagent.SendArgs
	if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if strings.TrimSpace(params.Message) == "" {
		return tools.ResultError("message is required"), nil
	}

	var mode subagent.MessageMode
	switch strings.TrimSpace(params.Mode) {
	case "", "followup":
		mode = subagent.MessageModeFollowUp
	case string(subagent.MessageModeSteer):
		mode = subagent.MessageModeSteer
	default:
		return tools.ResultError(fmt.Sprintf("invalid mode %q: must be empty/\"followup\" or %q", params.Mode, subagent.MessageModeSteer)), nil
	}

	id, _, errRes := r.resolveSubagentRef(sess, params.SubAgentID)
	if errRes != nil {
		return errRes, nil
	}

	if err := r.subagents.Send(id, subagent.Message{Content: params.Message, Mode: mode}); err != nil {
		return tools.ResultError(err.Error()), nil
	}
	updated, err := r.subagents.Get(id)
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}

	short := subagent.ShortRef(id)
	events.Emit(SubAgentSent(id, params.Message, sess.ID))
	// Fan a tree-change notification to every ancestor above the current
	// session so grandparent sidebars see the grandchild flip back to
	// "running" immediately. The direct parent already received
	// SubAgentSentEvent above; publishTreeChangeFromChild skips the
	// immediate parent, so there is no duplicate.
	r.publishTreeChangeFromChild(id, sess.ID)
	return tools.ResultJSON(map[string]string{
		"subagent_id": short,
		"agent":       updated.AgentName,
		"status":      updated.Status.String(),
	}), nil
}

// handleSubagentList returns a compact JSON snapshot of all subagents owned
// by the current session.
func (r *LocalRuntime) handleSubagentList(_ context.Context, sess *session.Session, _ tools.ToolCall, _ EventSink) (*tools.ToolCallResult, error) {
	snaps := r.subagents.ListParent(sess.ID)
	if len(snaps) == 0 {
		return tools.ResultSuccess("No active subagents for this session."), nil
	}
	out := make([]map[string]any, 0, len(snaps))
	for _, s := range snaps {
		out = append(out, map[string]any{
			"subagent_id":  subagent.ShortRef(s.ID),
			"agent":        s.AgentName,
			"status":       s.Status.String(),
			"created_at":   s.CreatedAt.Format(time.RFC3339),
			"last_preview": s.LastPreview,
		})
	}
	return tools.ResultJSON(out), nil
}

// handleSubagentInspect returns the last assistant message and, when
// explicitly requested, a recent slice or full non-system transcript.
func (r *LocalRuntime) handleSubagentInspect(_ context.Context, sess *session.Session, toolCall tools.ToolCall, _ EventSink) (*tools.ToolCallResult, error) {
	var params subagent.InspectArgs
	if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	id, snap, errRes := r.resolveSubagentRef(sess, params.SubAgentID)
	if errRes != nil {
		return errRes, nil
	}
	childSess, err := r.subagents.Session(id)
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}

	mode := subagent.NormalizeInspectMode(strings.TrimSpace(params.Mode))
	out := map[string]any{
		"subagent_id": subagent.ShortRef(snap.ID),
		"agent":       snap.AgentName,
		"status":      snap.Status.String(),
		"last":        childSess.GetLastAssistantMessageContent(),
		"mode":        mode,
	}

	switch mode {
	case subagent.InspectModeRecent:
		out["recent"] = collectSubagentMessages(childSess, subagent.InspectRecentLimit)
	case subagent.InspectModeFull:
		msgs := collectSubagentMessages(childSess, 0)
		trimmed, omitted, truncated := truncateInspectMessages(msgs, subagent.InspectFullMaxBytes)
		out["recent"] = trimmed
		if truncated {
			out["truncated"] = true
			out["omitted_messages"] = omitted
		}
	}

	return tools.ResultJSON(out), nil
}

func collectSubagentMessages(childSess *session.Session, limit int) []map[string]string {
	msgs := childSess.GetAllMessages()
	nonSystem := make([]map[string]string, 0, len(msgs))
	for _, m := range msgs {
		if m.Message.Role == chat.MessageRoleSystem {
			continue
		}
		nonSystem = append(nonSystem, map[string]string{
			"role":    string(m.Message.Role),
			"content": m.Message.Content,
		})
	}
	if limit > 0 && len(nonSystem) > limit {
		return nonSystem[len(nonSystem)-limit:]
	}
	return nonSystem
}

func truncateInspectMessages(msgs []map[string]string, maxBytes int) ([]map[string]string, int, bool) {
	if maxBytes <= 0 || len(msgs) == 0 {
		return msgs, 0, false
	}

	sizes := make([]int, len(msgs))
	for i, msg := range msgs {
		b, err := json.Marshal(msg)
		if err != nil {
			return msgs, 0, false
		}
		sizes[i] = len(b)
	}

	prefix := make([]int, len(sizes)+1)
	for i, size := range sizes {
		prefix[i+1] = prefix[i] + size
	}
	arraySize := func(from, count int) int {
		if count == 0 {
			return 2
		}
		return 2 + (prefix[from+count] - prefix[from]) + (count - 1)
	}

	total := arraySize(0, len(msgs))
	if total <= maxBytes {
		return msgs, 0, false
	}

	omitted := 0
	for omitted < len(msgs) {
		remaining := len(msgs) - omitted
		if arraySize(omitted, remaining) <= maxBytes {
			break
		}
		omitted++
	}
	if omitted >= len(msgs) {
		return []map[string]string{}, omitted, true
	}
	return msgs[omitted:], omitted, true
}

// handleSubagentFinalize asks the manager to close the subagent at its next
// safe point. Also used as the handler for the deprecated subagent_close.
func (r *LocalRuntime) handleSubagentFinalize(_ context.Context, sess *session.Session, toolCall tools.ToolCall, _ EventSink) (*tools.ToolCallResult, error) {
	var params subagent.FinalizeArgs
	if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	id, _, errRes := r.resolveSubagentRef(sess, params.SubAgentID)
	if errRes != nil {
		return errRes, nil
	}
	if err := r.subagents.Close(id); err != nil {
		return tools.ResultError(err.Error()), nil
	}
	return tools.ResultSuccess(fmt.Sprintf("Finalize requested for subagent %s.", subagent.ShortRef(id))), nil
}

// handleSubagentStop cancels a subagent immediately.
func (r *LocalRuntime) handleSubagentStop(_ context.Context, sess *session.Session, toolCall tools.ToolCall, _ EventSink) (*tools.ToolCallResult, error) {
	var params subagent.StopArgs
	if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	id, _, errRes := r.resolveSubagentRef(sess, params.SubAgentID)
	if errRes != nil {
		return errRes, nil
	}
	if err := r.subagents.Stop(id); err != nil {
		return tools.ResultError(err.Error()), nil
	}
	return tools.ResultSuccess(fmt.Sprintf("Subagent %s stopped.", subagent.ShortRef(id))), nil
}
