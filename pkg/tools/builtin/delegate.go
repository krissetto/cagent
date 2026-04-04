package builtin

import (
	"context"

	"github.com/docker/docker-agent/pkg/tools"
)

const (
	ToolNameDelegate        = "delegate"
	ToolNameListDelegations = "list_delegations"
	ToolNameViewDelegation  = "view_delegation"
	ToolNameStopDelegation  = "stop_delegation"
)

// DelegateMode controls how the delegation should execute.
type DelegateMode string

const (
	DelegateModeAsync   DelegateMode = "async"
	DelegateModeSync    DelegateMode = "sync"
	DelegateModeHandoff DelegateMode = "handoff"
)

type DelegateArgs struct {
	Agent          string       `json:"agent" jsonschema:"The name of the sub-agent to delegate to."`
	Task           string       `json:"task" jsonschema:"A clear and concise description of the task the agent should achieve."`
	ExpectedOutput string       `json:"expected_output,omitempty" jsonschema:"The expected output from the agent (optional)."`
	Mode           DelegateMode `json:"mode,omitempty" jsonschema:"Delegation mode: async (default), sync, or handoff."`
}

type ViewDelegationArgs struct {
	DelegationID string `json:"delegation_id" jsonschema:"The ID of the delegation to inspect."`
}

type StopDelegationArgs struct {
	DelegationID string `json:"delegation_id" jsonschema:"The ID of the delegation to stop."`
}

type DelegateTool struct{}

var _ tools.ToolSet = (*DelegateTool)(nil)

func NewDelegateTool() *DelegateTool {
	return &DelegateTool{}
}

func (t *DelegateTool) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{
		{
			Name:     ToolNameDelegate,
			Category: "transfer",
			Description: `Delegate work to a sub-agent. By default delegations run in the background and return immediately with a delegation ID.
Use mode="sync" to wait for the result inline, or mode="handoff" to transfer conversation ownership to another agent.`,
			Parameters: tools.MustSchemaFor[DelegateArgs](),
			Annotations: tools.ToolAnnotations{
				Title: "Delegate Task",
			},
		},
		{
			Name:        ToolNameListDelegations,
			Category:    "transfer",
			Description: `List all delegations with their status, mode, and runtime.`,
			Annotations: tools.ToolAnnotations{
				Title:        "List Delegations",
				ReadOnlyHint: true,
			},
		},
		{
			Name:        ToolNameViewDelegation,
			Category:    "transfer",
			Description: `View the output and status of a specific delegation by delegation ID.`,
			Parameters:  tools.MustSchemaFor[ViewDelegationArgs](),
			Annotations: tools.ToolAnnotations{
				Title:        "View Delegation",
				ReadOnlyHint: true,
			},
		},
		{
			Name:        ToolNameStopDelegation,
			Category:    "transfer",
			Description: `Stop a running delegation by delegation ID.`,
			Parameters:  tools.MustSchemaFor[StopDelegationArgs](),
			Annotations: tools.ToolAnnotations{
				Title: "Stop Delegation",
			},
		},
	}, nil
}

func (t *DelegateTool) Instructions() string {
	return `# Delegation

Use delegation to dispatch work to sub-agents.

- **delegate**: Delegate a task to a sub-agent. Default mode is async and returns immediately with a delegation ID.
- **list_delegations**: Show all delegations with status and runtime.
- **view_delegation**: Inspect a delegation by delegation_id.
- **stop_delegation**: Cancel a running delegation by delegation_id.

Modes:
- async: run in background and notify parent when complete
- sync: block until the delegated agent completes and return the result inline
- handoff: switch conversation ownership to another agent

Backward-compatible aliases remain available: transfer_task, handoff, run_background_agent, list_background_agents, view_background_agent, stop_background_agent.`
}
