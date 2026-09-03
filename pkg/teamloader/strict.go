package teamloader

import (
	"fmt"
	"strings"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider"
)

// checkRequirements enforces WithStrict: every provider, toolset type and
// feature the config relies on must be enabled by the registries and feature
// list the caller supplied. Agents on the `auto` model are resolved eagerly so
// the provider they land on is checked too.
func checkRequirements(cfg *latest.Config, opts *loadOptions, autoModel func() latest.ModelConfig) error {
	reqs := config.Requires(cfg)

	for _, a := range cfg.Agents {
		if a.Harness != nil {
			continue
		}
		for name := range strings.SplitSeq(a.Model, ",") {
			if name != "auto" {
				continue
			}
			m := autoModel()
			resolved := provider.ResolveType(&m, cfg.Providers)
			loc := fmt.Sprintf("agents.%s.model (auto → %s/%s)", a.Name, m.Provider, m.Model)
			reqs.Providers[resolved] = append(reqs.Providers[resolved], loc)
		}
	}

	return reqs.Check(opts.providerRegistry.Has, opts.toolsetRegistry.Has, opts.features)
}
