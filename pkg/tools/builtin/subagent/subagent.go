package subagent

import (
	"context"

	"github.com/docker/docker-agent/pkg/tools"
)

const (
	ToolNameSubagentStart    = "subagent_start"
	ToolNameSubagentSend     = "subagent_send"
	ToolNameSubagentInspect  = "subagent_inspect"
	ToolNameSubagentList     = "subagent_list"
	ToolNameSubagentStop     = "subagent_stop"
	ToolNameSubagentFinalize = "subagent_finalize"
)

type StartArgs struct {
	Agent string `json:"agent" jsonschema:"The name of the runtime-managed subagent to start."`
	Task  string `json:"task" jsonschema:"A clear and concise description of the task the subagent should begin with."`
}

type SendArgs struct {
	SubagentID string `json:"subagent_id" jsonschema:"The child session ID, or a unique prefix of it."`
	Message    string `json:"message" jsonschema:"The message to send to the runtime-managed subagent."`
	Mode       string `json:"mode,omitempty" jsonschema:"How to deliver the message: followup (default) or steer."`
}

type InspectArgs struct {
	SubagentID string `json:"subagent_id" jsonschema:"The child session ID, or a unique prefix of it."`
	Mode       string `json:"mode,omitempty" jsonschema:"How much transcript context to return: last (default), recent, or full."`
}

type TargetArgs struct {
	SubagentID string `json:"subagent_id" jsonschema:"The child session ID, or a unique prefix of it."`
}

func CreateToolSet() (tools.ToolSet, error) { return New(), nil }

func New() tools.ToolSet { return &ToolSet{} }

type ToolSet struct{}

func (t *ToolSet) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{
		{
			Name:        ToolNameSubagentStart,
			Category:    "transfer",
			Description: "Start a runtime-managed subagent in the background and return its child session ID.",
			Parameters:  tools.MustSchemaFor[StartArgs](),
			Annotations: tools.ToolAnnotations{Title: "Start Subagent"},
		},
		{
			Name:        ToolNameSubagentSend,
			Category:    "transfer",
			Description: "Send a follow-up or steer message to a live runtime-managed subagent.",
			Parameters:  tools.MustSchemaFor[SendArgs](),
			Annotations: tools.ToolAnnotations{Title: "Send to Subagent"},
		},
		{
			Name:        ToolNameSubagentInspect,
			Category:    "transfer",
			Description: "Inspect the latest, recent, or full transcript for a runtime-managed subagent.",
			Parameters:  tools.MustSchemaFor[InspectArgs](),
			Annotations: tools.ToolAnnotations{Title: "Inspect Subagent", ReadOnlyHint: true},
		},
		{
			Name:        ToolNameSubagentList,
			Category:    "transfer",
			Description: "List runtime-managed subagents in the current session tree with live statuses.",
			Annotations: tools.ToolAnnotations{Title: "List Subagents", ReadOnlyHint: true},
		},
		{
			Name:        ToolNameSubagentStop,
			Category:    "transfer",
			Description: "Immediately stop a running runtime-managed subagent.",
			Parameters:  tools.MustSchemaFor[TargetArgs](),
			Annotations: tools.ToolAnnotations{Title: "Stop Subagent"},
		},
		{
			Name:        ToolNameSubagentFinalize,
			Category:    "transfer",
			Description: "Ask a running runtime-managed subagent to finalize cleanly after its current safe point.",
			Parameters:  tools.MustSchemaFor[TargetArgs](),
			Annotations: tools.ToolAnnotations{Title: "Finalize Subagent"},
		},
	}, nil
}

func (t *ToolSet) Instructions() string {
	return `# Runtime-managed subagents

Use runtime-managed subagents for scoped background work that should continue independently and report through durable runtime events.

- subagent_start: start a child session and return its ID.
- subagent_send: send follow-up work or steer a live child session.
- subagent_inspect: inspect latest, recent, or full child transcript context.
- subagent_list: list live child sessions in the current tree.
- subagent_stop: cancel a running child immediately.
- subagent_finalize: ask a child to finish cleanly after its current safe point.`
}
