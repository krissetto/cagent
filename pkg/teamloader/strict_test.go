package teamloader

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/anthropic"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/think"
)

const strictYAML = `
models:
  main:
    provider: anthropic
    model: claude-sonnet-4-5
agents:
  root:
    model: main
    instruction: hi
    toolsets:
      - type: think
      - type: shell
    fallback:
      models: [openai/gpt-4o]
    hooks:
      pre_tool_use:
        - matcher: shell
          hooks:
            - type: command
              command: echo
`

func strictTestOpts(extra ...Opt) []Opt {
	anthropicOnly := provider.NewRegistry(map[string]provider.Factory{
		"anthropic": func(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (provider.Provider, error) {
			return anthropic.NewClient(ctx, cfg, env, opts...)
		},
	})
	thinkOnly := NewToolsetRegistry(map[string]ToolsetCreator{
		"think": func(context.Context, latest.Toolset, string, *config.RuntimeConfig, string) (tools.ToolSet, error) {
			return think.CreateToolSet()
		},
	})
	return append([]Opt{WithProviderRegistry(anthropicOnly), WithToolsetRegistry(thinkOnly)}, extra...)
}

func strictRunConfig() *config.RuntimeConfig {
	return &config.RuntimeConfig{
		EnvProviderForTests: environment.NewEnvListProvider([]string{"ANTHROPIC_API_KEY=dummy", "OPENAI_API_KEY=dummy"}),
	}
}

func TestLoadStrict_RejectsEverythingUnsupportedAtOnce(t *testing.T) {
	t.Parallel()

	_, err := Load(t.Context(), config.NewBytesSource("agent.yaml", []byte(strictYAML)), strictRunConfig(), strictTestOpts(WithStrict())...)
	require.Error(t, err)

	var unsupported *config.UnsupportedError
	require.ErrorAs(t, err, &unsupported)
	assert.Contains(t, err.Error(), `provider "openai" at agents.root.fallback.models[0]`)
	assert.Contains(t, err.Error(), `toolset type "shell" at agents.root.toolsets[1]`)
	assert.Contains(t, err.Error(), `feature "hooks" at agents.root.hooks`)
	assert.NotContains(t, err.Error(), `"anthropic"`)
	assert.NotContains(t, err.Error(), `"think"`)
}

func TestLoadStrict_PassesWhenEverythingIsEnabled(t *testing.T) {
	t.Parallel()

	yaml := `
models:
  main:
    provider: anthropic
    model: claude-sonnet-4-5
agents:
  root:
    model: main
    instruction: hi
    toolsets:
      - type: think
    hooks:
      pre_tool_use:
        - matcher: shell
          hooks:
            - type: command
              command: echo
`
	team, err := Load(t.Context(), config.NewBytesSource("agent.yaml", []byte(yaml)), strictRunConfig(), strictTestOpts(WithStrict(config.FeatureHooks))...)
	require.NoError(t, err)
	require.NotNil(t, team)
}

func TestLoadNonStrict_UnknownToolsetIsOnlyAWarning(t *testing.T) {
	t.Parallel()

	yaml := `
models:
  main:
    provider: anthropic
    model: claude-sonnet-4-5
agents:
  root:
    model: main
    instruction: hi
    toolsets:
      - type: shell
`
	team, err := Load(t.Context(), config.NewBytesSource("agent.yaml", []byte(yaml)), strictRunConfig(), strictTestOpts()...)
	require.NoError(t, err)
	root, err := team.Agent("root")
	require.NoError(t, err)
	warnings := root.DrainWarnings()
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "unknown toolset type: shell")
}

func TestLoadStrict_ResolvesAutoModelProvider(t *testing.T) {
	t.Parallel()

	yaml := `
agents:
  root:
    model: auto
    instruction: hi
`
	// Only OPENAI_API_KEY is set, so `auto` lands on openai, which the
	// anthropic-only registry cannot serve.
	runConfig := &config.RuntimeConfig{
		EnvProviderForTests: environment.NewEnvListProvider([]string{"OPENAI_API_KEY=dummy"}),
	}
	_, err := Load(t.Context(), config.NewBytesSource("agent.yaml", []byte(yaml)), runConfig, strictTestOpts(WithStrict())...)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `provider "openai" at agents.root.model (auto → openai/`)
}
