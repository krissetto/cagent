package subagents

import (
	"context"
	"fmt"
	"strings"

	"github.com/docker/docker-agent/pkg/agent"
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
	Agent string `json:"agent" jsonschema:"The name of the configured subagent to start."`
	Task  string `json:"task" jsonschema:"A clear, self-contained task for the subagent."`
}

type RefArgs struct {
	SubagentID string `json:"subagent_id" jsonschema:"The id or short id of the runtime-managed subagent."`
}

type SendArgs struct {
	SubagentID string `json:"subagent_id" jsonschema:"The id or short id of the runtime-managed subagent."`
	Message    string `json:"message" jsonschema:"The follow-up message to send to the subagent."`
}

type InspectArgs struct {
	SubagentID string `json:"subagent_id" jsonschema:"The id or short id of the runtime-managed subagent."`
	Mode       string `json:"mode,omitempty" jsonschema:"How much context to return: last, recent, or full."`
}

type ToolSet struct {
	agent *agent.Agent
}

func New(a *agent.Agent) *ToolSet {
	return &ToolSet{agent: a}
}

func (s *ToolSet) SetAgent(a *agent.Agent) {
	s.agent = a
}

func (s *ToolSet) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{
		{
			Name:        ToolNameSubagentStart,
			Category:    "subagents",
			Description: s.toolDescription("Start a runtime-managed subagent in the background. The subagent gets its own session and reports back through automatic runtime updates. Use this to parallelize independent work with a clear owner."),
			Parameters:  tools.MustSchemaFor[StartArgs](),
			Annotations: tools.ToolAnnotations{ReadOnlyHint: true, Title: "Start subagent"},
		},
		{
			Name:        ToolNameSubagentSend,
			Category:    "subagents",
			Description: "Send a follow-up message to a live runtime-managed subagent.",
			Parameters:  tools.MustSchemaFor[SendArgs](),
			Annotations: tools.ToolAnnotations{ReadOnlyHint: true, Title: "Send to subagent"},
		},
		{
			Name:        ToolNameSubagentInspect,
			Category:    "subagents",
			Description: "Read context from a runtime-managed subagent session after an update when more detail is needed.",
			Parameters:  tools.MustSchemaFor[InspectArgs](),
			Annotations: tools.ToolAnnotations{ReadOnlyHint: true, Title: "Inspect subagent"},
		},
		{
			Name:        ToolNameSubagentList,
			Category:    "subagents",
			Description: "List runtime-managed subagents for the current parent session. Use this to recover ids, not for polling progress.",
			Parameters:  tools.MustSchemaFor[struct{}](),
			Annotations: tools.ToolAnnotations{ReadOnlyHint: true, Title: "List subagents"},
		},
		{
			Name:        ToolNameSubagentStop,
			Category:    "subagents",
			Description: "Immediately stop a runtime-managed subagent.",
			Parameters:  tools.MustSchemaFor[RefArgs](),
			Annotations: tools.ToolAnnotations{ReadOnlyHint: true, Title: "Stop subagent"},
		},
		{
			Name:        ToolNameSubagentFinalize,
			Category:    "subagents",
			Description: "Ask a runtime-managed subagent to finish cleanly after its current safe point.",
			Parameters:  tools.MustSchemaFor[RefArgs](),
			Annotations: tools.ToolAnnotations{ReadOnlyHint: true, Title: "Finalize subagent"},
		},
	}, nil
}

func (s *ToolSet) toolDescription(base string) string {
	if s == nil || s.agent == nil {
		return base
	}
	return fmt.Sprintf("%s\n\nAvailable subagents:\n%s", base, specsDescription(s.agent))
}

func specsDescription(a *agent.Agent) string {
	var b strings.Builder
	for _, spec := range a.SubAgentSpecs() {
		description := spec.Description
		if description == "" {
			if subAgent, _, ok := a.SubAgentForName(spec.Name); ok && subAgent != nil {
				description = subAgent.Description()
			}
		}
		b.WriteString("- ")
		b.WriteString(spec.Name)
		if description != "" {
			b.WriteString(": ")
			b.WriteString(description)
		}
		b.WriteString("\n")
	}
	return b.String()
}
