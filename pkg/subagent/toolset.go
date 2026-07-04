package subagent

import (
	"context"
	"fmt"
	"strings"

	"github.com/docker/docker-agent/pkg/tools"
)

// ToolSet exposes the core subagent tools and a per-agent harness prompt. It is
// injected automatically onto any agent that declares `subagents:` — there is
// no user-facing toolset type to configure. It is built with the agent's
// allow-list so its Instructions list exactly the subagents the model may
// spawn.
type ToolSet struct {
	allowed []AllowedSubagent
}

// NewToolSet builds the subagent ToolSet for an agent with the given allow-list.
func NewToolSet(allowed []AllowedSubagent) *ToolSet {
	return &ToolSet{allowed: allowed}
}

// Tools returns the core subagent tool definitions.
func (t *ToolSet) Tools(context.Context) ([]tools.Tool, error) {
	return Definitions(), nil
}

// Instructions returns the harness guidance plus the list of subagents this
// agent may spawn, so the model knows exactly what it can delegate to.
func (t *ToolSet) Instructions() string {
	var b strings.Builder
	b.WriteString(Instructions())
	if len(t.allowed) > 0 {
		b.WriteString("\n\nYour subagents (use these names with spawn_subagent):\n")
		for _, a := range t.allowed {
			desc := a.Description
			if desc == "" {
				desc = "(no description)"
			}
			fmt.Fprintf(&b, "- %s: %s\n", a.DisplayName(), desc)
		}
	}
	return strings.TrimSpace(b.String())
}
