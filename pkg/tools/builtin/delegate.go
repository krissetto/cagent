package builtin

import (
	"context"

	"github.com/docker/docker-agent/pkg/tools"
)

const (
	ToolNameDelegate       = "delegate"
	ToolNameStopDelegation = "stop_delegation"
)

// DelegateArgs specifies the parameters for the delegate tool.
type DelegateArgs struct {
	Agent        string `json:"agent,omitempty" jsonschema:"description,The name of the sub-agent to delegate to. Required for new delegations."`
	DelegationID string `json:"delegation_id,omitempty" jsonschema:"description,The short 5-character delegation ID of an existing delegation. Omit to start new, provide to continue."`
	Message      string `json:"message" jsonschema:"description,The message to send to the agent."`
}

// StopDelegationArgs specifies the parameters for the stop_delegation tool.
type StopDelegationArgs struct {
	DelegationID string `json:"delegation_id" jsonschema:"description,The short 5-character delegation ID of the delegation to cancel."`
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
			Description: `Start or continue a conversation with a sub-agent. Provide agent + message to start a new delegation, or delegation_id + message to continue an existing one. Delegation IDs are short 5-character codes. Returns the delegation_id and the child agent's latest reply.`,
			Parameters:  tools.MustSchemaFor[DelegateArgs](),
			Annotations: tools.ToolAnnotations{Title: "Delegate"},
		},
		{
			Name:        ToolNameStopDelegation,
			Category:    "transfer",
			Description: `Stop a running delegation by delegation_id.`,
			Parameters:  tools.MustSchemaFor[StopDelegationArgs](),
			Annotations: tools.ToolAnnotations{Title: "Stop Delegation"},
		},
	}, nil
}

func (t *DelegateTool) Instructions() string {
	return "# Delegation\n\n" +
		"Use `delegate` to start or continue a conversation with a sub-agent.\n\n" +
		"- Start a new delegation with `agent` and `message`.\n" +
		"- Continue an existing delegation with `delegation_id` and `message`.\n" +
		"- The tool returns `{delegation_id, response}`.\n" +
		"- Use `stop_delegation` to cancel a running delegation."
}
