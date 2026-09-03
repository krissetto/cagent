// Package defaults wires docker-agent's full set of toolsets, model providers
// and agent sources into the team loader. Embedders that want a smaller
// binary pass their own registries to teamloader.Load instead of importing
// this package.
package defaults

import (
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/sources"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/providers"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/teamloader/toolsets"
)

func Opts() []teamloader.Opt {
	return []teamloader.Opt{
		teamloader.WithToolsetRegistry(toolsets.NewDefaultToolsetRegistry()),
		teamloader.WithProviderRegistry(providers.NewDefaultRegistry()),
		teamloader.WithSourceResolver(resolveExternalAgent),
	}
}

// resolveExternalAgent resolves sub-agent references with every source type;
// external agents are never signature-verified, so no OCI options apply.
func resolveExternalAgent(ref string, env environment.Provider) (config.Source, error) {
	return sources.Resolve(ref, env)
}
