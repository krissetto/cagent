package builtin

import (
	"context"

	"github.com/docker/docker-agent/pkg/tools"
)

const (
	ToolNameDelegate           = "delegate"
	ToolNameContinueDelegation = "continue_delegation"
	ToolNameStopDelegation     = "stop_delegation"
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
	}, nil
}

func (t *DelegateTool) Instructions() string {
	return "# Delegation\n\n" +
		"Use `delegate` to start a background sub-agent run. It returns immediately with a `delegation_id` and `status` \"started\"; it does not wait for a reply.\n\n" +
		"Use `continue_delegation` with a `delegation_id` to send a follow-up message to the same agent session; it returns immediately and the agent's reply will be delivered when ready.\n\n" +
		"Use `stop_delegation` to cancel a running delegation that is no longer needed.\n\n" +
		"After calling `delegate`, either continue your own work or come back later with `continue_delegation` if you need a follow-up reply from that child session.\n\n" +
		"Never pass `delegation_id` to `delegate` — use `continue_delegation` for existing delegations."
}
