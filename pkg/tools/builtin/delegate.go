package builtin

import (
	"context"

	"github.com/docker/docker-agent/pkg/tools"
)

const (
	ToolNameDelegate             = "delegate"
	ToolNameContinueDelegation   = "continue_delegation"
	ToolNameStopDelegation       = "stop_delegation"
	ToolNameGetDelegationResult  = "get_delegation_result"
)

// DelegateArgs is for starting a new delegation.
type DelegateArgs struct {
	Agent string `json:"agent" jsonschema:"The name of the sub-agent to delegate to."`
	Task  string `json:"task"  jsonschema:"The task or instructions to send to the sub-agent."`
}

// ContinueDelegationArgs is for sending a follow-up message to an existing delegation.
type ContinueDelegationArgs struct {
	DelegationID string `json:"delegation_id" jsonschema:"The short 5-character delegation ID returned by the delegate tool."`
	Message      string `json:"message"       jsonschema:"The follow-up message to send to the sub-agent."`
}

// StopDelegationArgs specifies the parameters for the stop_delegation tool.
type StopDelegationArgs struct {
	DelegationID string `json:"delegation_id" jsonschema:"The short 5-character delegation ID of the delegation to cancel."`
}

// GetDelegationResultArgs specifies the delegation to retrieve the result for.
type GetDelegationResultArgs struct {
	DelegationID string `json:"delegation_id" jsonschema:"The short 5-character delegation ID returned by the delegate tool."`
}

type DelegateTool struct{}

var _ tools.ToolSet = (*DelegateTool)(nil)

func NewDelegateTool() *DelegateTool {
	return &DelegateTool{}
}

func (t *DelegateTool) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{
		{
			Name:        ToolNameDelegate,
			Category:    "transfer",
			Description: `Start a new background delegation — assign a task to a sub-agent and return immediately with a delegation_id and status "started".`,
			Parameters:  tools.MustSchemaFor[DelegateArgs](),
			Annotations: tools.ToolAnnotations{Title: "Delegate"},
		},
		{
			Name:        ToolNameContinueDelegation,
			Category:    "transfer",
			Description: `Send a follow-up message to an existing delegation using its delegation_id. Returns immediately; the agent processes the message in the background.`,
			Parameters:  tools.MustSchemaFor[ContinueDelegationArgs](),
			Annotations: tools.ToolAnnotations{Title: "Continue Delegation"},
		},
		{
			Name:        ToolNameStopDelegation,
			Category:    "transfer",
			Description: `Stop a running delegation by its delegation_id.`,
			Parameters:  tools.MustSchemaFor[StopDelegationArgs](),
			Annotations: tools.ToolAnnotations{Title: "Stop Delegation"},
		},
		{
			Name:        ToolNameGetDelegationResult,
			Category:    "transfer",
			Description: `Retrieve the current status and latest result of a background delegation by delegation_id, without copying the child session content into the parent transcript.`,
			Parameters:  tools.MustSchemaFor[GetDelegationResultArgs](),
			Annotations: tools.ToolAnnotations{Title: "Get Delegation Result", ReadOnlyHint: true},
		},
	}, nil
}

func (t *DelegateTool) Instructions() string {
	return "# Delegation\n\n" +
		"Use `delegate` to start a background sub-agent run. It returns immediately with a `delegation_id` and `status` \"started\"; it does not wait for a reply.\n\n" +
		"Use `continue_delegation` with a `delegation_id` to send a follow-up message to the same agent session; it returns immediately and the agent continues in the background.\n\n" +
		"Use `get_delegation_result` to inspect the current result or status of a background delegation without copying the child session's full content into the parent session.\n\n" +
		"Use `stop_delegation` to cancel a running delegation that is no longer needed.\n\n" +
		"When a background sub-agent finishes a turn, you may receive a short notification such as `agent_name (delegation_id) has responded`. Treat that as a prompt to inspect or continue that child session with tools, not as the child session's full content.\n\n" +
		"Never pass `delegation_id` to `delegate` — use `continue_delegation`, `get_delegation_result`, or `stop_delegation` for existing delegations."
}
