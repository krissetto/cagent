package mcpcatalog

import (
	"context"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/tools"
)

// Creator builds this toolset from its YAML declaration. It matches
// teamloader.ToolsetCreator so it can be registered directly:
//
//	teamloader.NewToolsetRegistry(map[string]teamloader.ToolsetCreator{"mcp_catalog": mcpcatalog.Creator})
func Creator(_ context.Context, toolset latest.Toolset, _ string, _ *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
	var opts []Option
	if len(toolset.AllowedServers) > 0 {
		opts = append(opts, WithAllowedServers(toolset.AllowedServers))
	}
	if len(toolset.BlockedServers) > 0 {
		opts = append(opts, WithBlockedServers(toolset.BlockedServers))
	}
	return New(opts...), nil
}
