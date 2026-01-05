package config

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/cagent/pkg/config/latest"
	"github.com/docker/cagent/pkg/environment"
)

func TestAutoRegisterModels(t *testing.T) {
	t.Parallel()

	cfg, err := Load(t.Context(), testfileSource("testdata/autoregister.yaml"))
	require.NoError(t, err)

	assert.Len(t, cfg.Models, 2)
	assert.Equal(t, "openai", cfg.Models["openai/gpt-4o"].Provider)
	assert.Equal(t, "gpt-4o", cfg.Models["openai/gpt-4o"].Model)
	assert.Equal(t, "anthropic", cfg.Models["anthropic/claude-sonnet-4-0"].Provider)
	assert.Equal(t, "claude-sonnet-4-0", cfg.Models["anthropic/claude-sonnet-4-0"].Model)
}

func TestAutoRegisterAlloy(t *testing.T) {
	t.Parallel()

	cfg, err := Load(t.Context(), testfileSource("testdata/autoregister_alloy.yaml"))
	require.NoError(t, err)

	assert.Len(t, cfg.Models, 2)
	assert.Equal(t, "openai", cfg.Models["openai/gpt-4o"].Provider)
	assert.Equal(t, "gpt-4o", cfg.Models["openai/gpt-4o"].Model)
	assert.Equal(t, "anthropic", cfg.Models["anthropic/claude-sonnet-4-0"].Provider)
	assert.Equal(t, "claude-sonnet-4-0", cfg.Models["anthropic/claude-sonnet-4-0"].Model)
}

func TestMigrate_v0_v1_provider(t *testing.T) {
	t.Parallel()

	cfg, err := Load(t.Context(), testfileSource("testdata/provider_v0.yaml"))
	require.NoError(t, err)

	assert.Equal(t, "openai", cfg.Models["gpt"].Provider)
}

func TestMigrate_v1_provider(t *testing.T) {
	t.Parallel()

	cfg, err := Load(t.Context(), testfileSource("testdata/provider_v1.yaml"))
	require.NoError(t, err)

	assert.Equal(t, "openai", cfg.Models["gpt"].Provider)
}

func TestMigrate_v0_v1_todo(t *testing.T) {
	t.Parallel()

	cfg, err := Load(t.Context(), testfileSource("testdata/todo_v0.yaml"))
	require.NoError(t, err)

	assert.Len(t, cfg.Agents["root"].Toolsets, 2)
	assert.Equal(t, "todo", cfg.Agents["root"].Toolsets[0].Type)
	assert.False(t, cfg.Agents["root"].Toolsets[0].Shared)
	assert.Equal(t, "mcp", cfg.Agents["root"].Toolsets[1].Type)
}

func TestMigrate_v1_todo(t *testing.T) {
	t.Parallel()

	cfg, err := Load(t.Context(), testfileSource("testdata/todo_v1.yaml"))
	require.NoError(t, err)

	assert.Len(t, cfg.Agents["root"].Toolsets, 2)
	assert.Equal(t, "todo", cfg.Agents["root"].Toolsets[0].Type)
	assert.False(t, cfg.Agents["root"].Toolsets[0].Shared)
	assert.Equal(t, "mcp", cfg.Agents["root"].Toolsets[1].Type)
}

func TestMigrate_v0_v1_shared_todo(t *testing.T) {
	t.Parallel()

	cfg, err := Load(t.Context(), testfileSource("testdata/shared_todo_v0.yaml"))
	require.NoError(t, err)

	assert.Len(t, cfg.Agents["root"].Toolsets, 2)
	assert.Equal(t, "todo", cfg.Agents["root"].Toolsets[0].Type)
	assert.True(t, cfg.Agents["root"].Toolsets[0].Shared)
	assert.Equal(t, "mcp", cfg.Agents["root"].Toolsets[1].Type)
}

func TestMigrate_v1_shared_todo(t *testing.T) {
	t.Parallel()

	cfg, err := Load(t.Context(), testfileSource("testdata/shared_todo_v1.yaml"))
	require.NoError(t, err)

	assert.Len(t, cfg.Agents["root"].Toolsets, 2)
	assert.Equal(t, "todo", cfg.Agents["root"].Toolsets[0].Type)
	assert.True(t, cfg.Agents["root"].Toolsets[0].Shared)
	assert.Equal(t, "mcp", cfg.Agents["root"].Toolsets[1].Type)
}

func TestMigrate_v0_v1_think(t *testing.T) {
	t.Parallel()

	cfg, err := Load(t.Context(), testfileSource("testdata/think_v0.yaml"))
	require.NoError(t, err)

	assert.Len(t, cfg.Agents["root"].Toolsets, 2)
	assert.Equal(t, "think", cfg.Agents["root"].Toolsets[0].Type)
	assert.Equal(t, "mcp", cfg.Agents["root"].Toolsets[1].Type)
}

func TestMigrate_v1_think(t *testing.T) {
	t.Parallel()

	cfg, err := Load(t.Context(), testfileSource("testdata/think_v1.yaml"))
	require.NoError(t, err)

	assert.Len(t, cfg.Agents["root"].Toolsets, 2)
	assert.Equal(t, "think", cfg.Agents["root"].Toolsets[0].Type)
	assert.Equal(t, "mcp", cfg.Agents["root"].Toolsets[1].Type)
}

func TestMigrate_v0_v1_memory(t *testing.T) {
	t.Parallel()

	cfg, err := Load(t.Context(), testfileSource("testdata/memory_v0.yaml"))
	require.NoError(t, err)

	assert.Len(t, cfg.Agents["root"].Toolsets, 2)
	assert.Equal(t, "memory", cfg.Agents["root"].Toolsets[0].Type)
	assert.Equal(t, "dev_memory.db", cfg.Agents["root"].Toolsets[0].Path)
	assert.Equal(t, "mcp", cfg.Agents["root"].Toolsets[1].Type)
}

func TestMigrate_v1_memory(t *testing.T) {
	t.Parallel()

	cfg, err := Load(t.Context(), testfileSource("testdata/memory_v1.yaml"))
	require.NoError(t, err)

	assert.Len(t, cfg.Agents["root"].Toolsets, 2)
	assert.Equal(t, "memory", cfg.Agents["root"].Toolsets[0].Type)
	assert.Equal(t, "dev_memory.db", cfg.Agents["root"].Toolsets[0].Path)
	assert.Equal(t, "mcp", cfg.Agents["root"].Toolsets[1].Type)
}

func TestMigrate_v1(t *testing.T) {
	t.Parallel()

	_, err := Load(t.Context(), testfileSource("testdata/v1.yaml"))
	require.NoError(t, err)
}

func openRoot(t *testing.T, dir string) *os.Root {
	t.Helper()

	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	t.Cleanup(func() { root.Close() })

	return root
}

type noEnvProvider struct{}

func (p *noEnvProvider) Get(context.Context, string) (string, bool) { return "", false }

func TestCheckRequiredEnvVars(t *testing.T) {
	t.Parallel()

	tests := []struct {
		yaml            string
		expectedMissing []string
	}{
		{
			yaml:            "openai_inline.yaml",
			expectedMissing: []string{"OPENAI_API_KEY"},
		},
		{
			yaml:            "anthropic_inline.yaml",
			expectedMissing: []string{"ANTHROPIC_API_KEY"},
		},
		{
			yaml:            "google_inline.yaml",
			expectedMissing: []string{"GOOGLE_API_KEY"},
		},
		{
			yaml:            "mistral_inline.yaml",
			expectedMissing: []string{"MISTRAL_API_KEY"},
		},
		{
			yaml:            "dmr_inline.yaml",
			expectedMissing: []string{},
		},
		{
			yaml:            "openai_model.yaml",
			expectedMissing: []string{"OPENAI_API_KEY"},
		},
		{
			yaml:            "anthropic_model.yaml",
			expectedMissing: []string{"ANTHROPIC_API_KEY"},
		},
		{
			yaml:            "google_model.yaml",
			expectedMissing: []string{"GOOGLE_API_KEY"},
		},
		{
			yaml:            "mistral_model.yaml",
			expectedMissing: []string{"MISTRAL_API_KEY"},
		},
		{
			yaml:            "dmr_model.yaml",
			expectedMissing: []string{},
		},
		{
			yaml:            "all.yaml",
			expectedMissing: []string{"ANTHROPIC_API_KEY", "GOOGLE_API_KEY", "MISTRAL_API_KEY", "OPENAI_API_KEY"},
		},
		{
			yaml:            "openai_and_unused_mistral_model.yaml",
			expectedMissing: []string{"OPENAI_API_KEY"},
		},
	}
	for _, test := range tests {
		t.Run(test.yaml, func(t *testing.T) {
			t.Parallel()

			cfg, err := Load(t.Context(), testfileSource("testdata/env/"+test.yaml))
			require.NoError(t, err)

			err = CheckRequiredEnvVars(t.Context(), cfg, "", &noEnvProvider{})

			if len(test.expectedMissing) == 0 {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Equal(t, test.expectedMissing, err.(*environment.RequiredEnvError).Missing)
			}
		})
	}
}

func TestCheckRequiredEnvVarsWithModelGateway(t *testing.T) {
	t.Parallel()

	cfg, err := Load(t.Context(), testfileSource("testdata/env/all.yaml"))
	require.NoError(t, err)

	err = CheckRequiredEnvVars(t.Context(), cfg, "gateway:8080", &noEnvProvider{})

	require.NoError(t, err)
}

func TestApplyModelOverrides(t *testing.T) {
	tests := []struct {
		name        string
		agents      map[string]latest.AgentConfig
		overrides   []string
		expected    map[string]string // agent name -> expected model
		expectError bool
		errorMsg    string
	}{
		{
			name: "global override",
			agents: map[string]latest.AgentConfig{
				"root":  {Model: "openai/gpt-4"},
				"other": {Model: "anthropic/claude-3"},
			},
			overrides: []string{"google/gemini-pro"},
			expected: map[string]string{
				"root":  "google/gemini-pro",
				"other": "google/gemini-pro",
			},
		},
		{
			name: "single per-agent override",
			agents: map[string]latest.AgentConfig{
				"root":  {Model: "openai/gpt-4"},
				"other": {Model: "anthropic/claude-3"},
			},
			overrides: []string{"other=google/gemini-pro"},
			expected: map[string]string{
				"root":  "openai/gpt-4",
				"other": "google/gemini-pro",
			},
		},
		{
			name: "multiple separate flags",
			agents: map[string]latest.AgentConfig{
				"root":  {Model: "openai/gpt-4"},
				"other": {Model: "anthropic/claude-3"},
			},
			overrides: []string{"root=openai/gpt-5", "other=anthropic/claude-sonnet-4-0"},
			expected: map[string]string{
				"root":  "openai/gpt-5",
				"other": "anthropic/claude-sonnet-4-0",
			},
		},
		{
			name: "comma-separated format",
			agents: map[string]latest.AgentConfig{
				"root":  {Model: "openai/gpt-4"},
				"other": {Model: "anthropic/claude-3"},
				"third": {Model: "google/gemini-pro"},
			},
			overrides: []string{"root=openai/gpt-5,other=anthropic/claude-sonnet-4-0"},
			expected: map[string]string{
				"root":  "openai/gpt-5",
				"other": "anthropic/claude-sonnet-4-0",
				"third": "google/gemini-pro",
			},
		},
		{
			name: "mixed formats",
			agents: map[string]latest.AgentConfig{
				"root":     {Model: "openai/gpt-4"},
				"other":    {Model: "anthropic/claude-3"},
				"third":    {Model: "google/gemini-pro"},
				"reviewer": {Model: "openai/gpt-3.5-turbo"},
			},
			overrides: []string{"root=openai/gpt-5,other=anthropic/claude-4", "reviewer=google/gemini-1.5-pro"},
			expected: map[string]string{
				"root":     "openai/gpt-5",
				"other":    "anthropic/claude-4",
				"third":    "google/gemini-pro",
				"reviewer": "google/gemini-1.5-pro",
			},
		},
		{
			name: "last override wins",
			agents: map[string]latest.AgentConfig{
				"root": {Model: "openai/gpt-4"},
			},
			overrides: []string{"root=openai/gpt-5", "root=anthropic/claude-4"},
			expected: map[string]string{
				"root": "anthropic/claude-4",
			},
		},
		{
			name: "unknown agent error",
			agents: map[string]latest.AgentConfig{
				"root": {Model: "openai/gpt-4"},
			},
			overrides:   []string{"nonexistent=openai/gpt-5"},
			expectError: true,
			errorMsg:    "unknown agent 'nonexistent'",
		},
		{
			name: "empty model spec error",
			agents: map[string]latest.AgentConfig{
				"root": {Model: "openai/gpt-4"},
			},
			overrides:   []string{"root="},
			expectError: true,
			errorMsg:    "empty model specification in override: root=",
		},
		{
			name: "empty global model spec is skipped",
			agents: map[string]latest.AgentConfig{
				"root": {Model: "openai/gpt-4"},
			},
			overrides: []string{""},
			expected: map[string]string{
				"root": "openai/gpt-4",
			},
		},
		{
			name: "whitespace handling",
			agents: map[string]latest.AgentConfig{
				"root":  {Model: "openai/gpt-4"},
				"other": {Model: "anthropic/claude-3"},
			},
			overrides: []string{" root = openai/gpt-5 , other = anthropic/claude-4 "},
			expected: map[string]string{
				"root":  "openai/gpt-5",
				"other": "anthropic/claude-4",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := &latest.Config{
				Agents: tt.agents,
				Models: make(map[string]latest.ModelConfig),
			}

			err := ApplyModelOverrides(cfg, tt.overrides)

			if tt.expectError {
				require.ErrorContains(t, err, tt.errorMsg)
			} else {
				require.NoError(t, err)
				for agentName, expectedModel := range tt.expected {
					assert.Equal(t, expectedModel, cfg.Agents[agentName].Model)
				}
			}
		})
	}
}

func TestProviders_DefaultsApplied(t *testing.T) {
	t.Parallel()

	cfg, err := Load(t.Context(), testfileSource("testdata/providers.yaml"))
	require.NoError(t, err)

	// Check that provider defaults are applied to custom_model
	customModel := cfg.Models["custom_model"]
	assert.Equal(t, "custom_provider", customModel.Provider)
	assert.Equal(t, "gpt-4o", customModel.Model)
	assert.Equal(t, "https://api.example.com/v1", customModel.BaseURL)
	assert.Equal(t, "CUSTOM_API_KEY", customModel.TokenKey)
	assert.Equal(t, "openai_chatcompletions", customModel.ProviderOpts["api_type"])
}

func TestProviders_ModelOverridesTakePrecedence(t *testing.T) {
	t.Parallel()

	cfg, err := Load(t.Context(), testfileSource("testdata/providers.yaml"))
	require.NoError(t, err)

	// Check that model-level overrides take precedence
	overrideModel := cfg.Models["model_with_overrides"]
	assert.Equal(t, "custom_provider", overrideModel.Provider)
	assert.Equal(t, "https://override.example.com/v1", overrideModel.BaseURL)
	assert.Equal(t, "OVERRIDE_API_KEY", overrideModel.TokenKey)
	// api_type should still be set from provider since model didn't override it
	assert.Equal(t, "openai_chatcompletions", overrideModel.ProviderOpts["api_type"])
}

func TestProviders_ShorthandSyntax(t *testing.T) {
	t.Parallel()

	cfg, err := Load(t.Context(), testfileSource("testdata/providers.yaml"))
	require.NoError(t, err)

	// Check that shorthand syntax creates a model entry with provider defaults
	shorthandModel, exists := cfg.Models["custom_provider/gpt-4o-turbo"]
	require.True(t, exists, "Shorthand model should be auto-registered")
	assert.Equal(t, "custom_provider", shorthandModel.Provider)
	assert.Equal(t, "gpt-4o-turbo", shorthandModel.Model)
	assert.Equal(t, "https://api.example.com/v1", shorthandModel.BaseURL)
	assert.Equal(t, "CUSTOM_API_KEY", shorthandModel.TokenKey)
	assert.Equal(t, "openai_chatcompletions", shorthandModel.ProviderOpts["api_type"])
}

func TestProviders_Validation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		providers map[string]latest.ProviderConfig
		wantErr   string
	}{
		{
			name: "valid provider",
			providers: map[string]latest.ProviderConfig{
				"my_provider": {
					APIType:  "openai_chatcompletions",
					BaseURL:  "https://api.example.com/v1",
					TokenKey: "MY_API_KEY",
				},
			},
			wantErr: "",
		},
		{
			name: "valid provider with responses api_type",
			providers: map[string]latest.ProviderConfig{
				"responses_provider": {
					APIType: "openai_responses",
					BaseURL: "https://api.example.com/v1",
				},
			},
			wantErr: "",
		},
		{
			name: "valid provider with empty api_type",
			providers: map[string]latest.ProviderConfig{
				"default_provider": {
					BaseURL: "https://api.example.com/v1",
				},
			},
			wantErr: "",
		},
		{
			name: "missing base_url",
			providers: map[string]latest.ProviderConfig{
				"no_base_url": {
					APIType: "openai_chatcompletions",
				},
			},
			wantErr: "base_url is required",
		},
		{
			name: "invalid api_type",
			providers: map[string]latest.ProviderConfig{
				"bad_provider": {
					APIType: "invalid_api_type",
					BaseURL: "https://api.example.com/v1",
				},
			},
			wantErr: "invalid api_type 'invalid_api_type'",
		},
		{
			name: "provider name with slash",
			providers: map[string]latest.ProviderConfig{
				"bad/name": {
					APIType: "openai_chatcompletions",
					BaseURL: "https://api.example.com/v1",
				},
			},
			wantErr: "name cannot contain '/'",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := &latest.Config{
				Providers: tt.providers,
				Agents: map[string]latest.AgentConfig{
					"root": {Model: "openai/gpt-4o"},
				},
			}

			err := validateConfig(cfg)

			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
