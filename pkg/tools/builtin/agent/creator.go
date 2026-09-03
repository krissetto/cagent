package agent

import (
	"context"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/tools"
)

// Creator builds this toolset from its YAML declaration. It matches
// teamloader.ToolsetCreator so it can be registered directly:
//
//	teamloader.NewToolsetRegistry(map[string]teamloader.ToolsetCreator{"background_agents": agent.Creator})
func Creator(context.Context, latest.Toolset, string, *config.RuntimeConfig, string) (tools.ToolSet, error) {
	return CreateToolSet()
}
