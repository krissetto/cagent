package teamloader

import (
	"errors"
	"fmt"
	"strings"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
)

func runtimeSubagentRefs(configSpecs latest.SubagentSpecs) ([]string, []agent.SubAgentSpec, error) {
	refs := make([]string, 0, len(configSpecs))
	specs := make([]agent.SubAgentSpec, 0, len(configSpecs))
	seenNames := make(map[string]struct{}, len(configSpecs))

	for _, configSpec := range configSpecs {
		name := strings.TrimSpace(configSpec.Name)
		backingAgent := strings.TrimSpace(configSpec.Agent)
		if name == "" {
			name = backingAgent
		}
		if backingAgent == "" {
			backingAgent = name
		}
		if name == "" {
			return nil, nil, errors.New("subagent entry must set name or agent")
		}
		if _, ok := seenNames[name]; ok {
			return nil, nil, fmt.Errorf("duplicate subagent name %q", name)
		}
		seenNames[name] = struct{}{}
		refs = append(refs, backingAgent)
		specs = append(specs, agent.SubAgentSpec{
			Name:        name,
			Agent:       backingAgent,
			Description: strings.TrimSpace(configSpec.Description),
		})
	}

	return refs, specs, nil
}
