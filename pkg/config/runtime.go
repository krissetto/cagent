package config

import (
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
)

type RuntimeConfig struct {
	Config

	EnvProviderForTests environment.Provider
	envProvider         environment.Provider
	envProviderLock     sync.Mutex
}

type Config struct {
	EnvFiles       []string
	ModelsGateway  string
	DefaultModel   *latest.ModelConfig
	GlobalCodeMode bool
	WorkingDir     string
	Models         map[string]latest.ModelConfig
	Providers      map[string]latest.ProviderConfig

	// SubagentIdleAutoFinalize configures optional auto-finalization for
	// stale idle subagents. Zero means disabled (default).
	SubagentIdleAutoFinalize time.Duration

	// Hook overrides from CLI flags
	HookPreToolUse   []string
	HookPostToolUse  []string
	HookSessionStart []string
	HookSessionEnd   []string
	HookOnUserInput  []string
}

func (runConfig *RuntimeConfig) Clone() *RuntimeConfig {
	clone := &RuntimeConfig{
		Config: runConfig.Config,
	}
	clone.EnvFiles = slices.Clone(runConfig.EnvFiles)
	clone.Models = maps.Clone(runConfig.Models)
	clone.Providers = maps.Clone(runConfig.Providers)
	clone.DefaultModel = runConfig.DefaultModel.Clone()
	clone.HookPreToolUse = slices.Clone(runConfig.HookPreToolUse)
	clone.HookPostToolUse = slices.Clone(runConfig.HookPostToolUse)
	clone.HookSessionStart = slices.Clone(runConfig.HookSessionStart)
	clone.HookSessionEnd = slices.Clone(runConfig.HookSessionEnd)
	clone.HookOnUserInput = slices.Clone(runConfig.HookOnUserInput)
	return clone
}

func (runConfig *RuntimeConfig) EnvProvider() environment.Provider {
	if runConfig.EnvProviderForTests != nil {
		return runConfig.EnvProviderForTests
	}

	runConfig.envProviderLock.Lock()
	defer runConfig.envProviderLock.Unlock()

	env := runConfig.computedEnvProvider()
	runConfig.envProvider = env
	return env
}

func (runConfig *RuntimeConfig) computedEnvProvider() environment.Provider {
	defaultEnv := environment.NewDefaultProvider()

	// Make env file paths absolute relative to the working directory.
	var err error
	runConfig.EnvFiles, err = environment.AbsolutePaths(runConfig.WorkingDir, runConfig.EnvFiles)
	if err != nil {
		slog.Error("Failed to make env file paths absolute", "error", err)
		return defaultEnv
	}

	envFilesProviders, err := environment.NewEnvFilesProvider(runConfig.EnvFiles)
	if err != nil {
		slog.Error("Failed to read env files", "error", err)
		return defaultEnv
	}

	// Update the env provider to include env files
	return environment.NewMultiProvider(envFilesProviders, defaultEnv)
}
