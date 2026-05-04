package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin"
)

// findModelPickerToolForAgent returns the ModelPickerTool from the given
// agent's toolsets, or nil if the agent has no model_picker configured.
func (r *LocalRuntime) findModelPickerToolForAgent(a *agent.Agent) *builtin.ModelPickerTool {
	if a == nil {
		return nil
	}
	for _, ts := range a.ToolSets() {
		if mpt, ok := tools.As[*builtin.ModelPickerTool](ts); ok {
			return mpt
		}
	}
	return nil
}

// handleChangeModel handles the change_model tool call by switching the
// session-resolved agent's model. It reads the agent identity from
// resolveSessionAgent(sess) so that child sessions pinned to a different
// agent change the child's model, not the root's.
func (r *LocalRuntime) handleChangeModel(ctx context.Context, _ *sessionRunner, sess *session.Session, toolCall tools.ToolCall, events chan Event) (*tools.ToolCallResult, error) {
	var params builtin.ChangeModelArgs
	if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	if params.Model == "" {
		return tools.ResultError("model parameter is required"), nil
	}

	a := r.resolveSessionAgent(sess)
	// Validate the requested model against the allowed list
	mpt := r.findModelPickerToolForAgent(a)
	if mpt == nil {
		return tools.ResultError("model_picker is not configured for this agent"), nil
	}
	allowed := mpt.AllowedModels()
	if !slices.Contains(allowed, params.Model) {
		return tools.ResultError(fmt.Sprintf(
			"model %q is not in the allowed list. Available models: %s",
			params.Model, strings.Join(allowed, ", "),
		)), nil
	}

	return r.setModelAndEmitInfo(ctx, a.Name(), params.Model, events)
}

// handleRevertModel handles the revert_model tool call by reverting the
// session-resolved agent to its default model.
func (r *LocalRuntime) handleRevertModel(ctx context.Context, _ *sessionRunner, sess *session.Session, _ tools.ToolCall, events chan Event) (*tools.ToolCallResult, error) {
	a := r.resolveSessionAgent(sess)
	return r.setModelAndEmitInfo(ctx, a.Name(), "", events)
}

// setModelAndEmitInfo sets the model for the named agent and emits an updated
// AgentInfo event so the UI reflects the change. An empty modelRef reverts to
// the agent's default model.
func (r *LocalRuntime) setModelAndEmitInfo(ctx context.Context, agentName, modelRef string, events chan Event) (*tools.ToolCallResult, error) {
	if err := r.SetAgentModel(ctx, agentName, modelRef); err != nil {
		return tools.ResultError(fmt.Sprintf("failed to set model: %v", err)), nil
	}

	if a, err := r.team.Agent(agentName); err == nil {
		events <- AgentInfo(a.Name(), r.getEffectiveModelID(a), a.Description(), a.WelcomeMessage())
	} else {
		slog.Warn("Failed to retrieve agent after model change; UI may not reflect the update", "agent", agentName, "error", err)
	}

	if modelRef == "" {
		slog.Info("Model reverted via model_picker tool", "agent", agentName)
		return tools.ResultSuccess("Model reverted to the agent's default model"), nil
	}
	slog.Info("Model changed via model_picker tool", "agent", agentName, "model", modelRef)
	return tools.ResultSuccess("Model changed to " + modelRef), nil
}
