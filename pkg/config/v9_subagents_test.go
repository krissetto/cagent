package config

import (
	"testing"

	yamlpkg "github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
)

// TestV9_SubagentsField verifies that the new schema v9 `subagents:` field
// parses correctly, both in string shorthand and full object form, while
// the legacy `sub_agents:` field continues to work unchanged.
func TestV9_SubagentsField(t *testing.T) {
	t.Parallel()

	t.Run("v9 subagents from file", func(t *testing.T) {
		t.Parallel()

		cfg, err := Load(t.Context(), NewFileSource("testdata/v9_subagents.yaml"))
		require.NoError(t, err)
		require.Equal(t, "9", cfg.Version)

		root, ok := cfg.Agents.Lookup("root")
		require.True(t, ok)

		// subagents field should be populated
		require.Len(t, root.Subagents, 2)
		assert.Equal(t, "researcher", root.Subagents[0].Agent)
		assert.Empty(t, root.Subagents[0].Description)
		assert.Equal(t, "writer", root.Subagents[1].Agent)
		assert.Equal(t, "Drafts the final document from research notes", root.Subagents[1].Description)

		// sub_agents should be empty (not used here)
		assert.Empty(t, root.SubAgents)
	})

	t.Run("v9 sub_agents from file", func(t *testing.T) {
		t.Parallel()

		cfg, err := Load(t.Context(), NewFileSource("testdata/v9_sub_agents.yaml"))
		require.NoError(t, err)
		require.Equal(t, "9", cfg.Version)

		root, ok := cfg.Agents.Lookup("root")
		require.True(t, ok)

		// Legacy sub_agents field is preserved
		require.Len(t, root.SubAgents, 1)
		assert.Equal(t, "helper", root.SubAgents[0])

		// subagents (runtime-managed) is empty
		assert.Empty(t, root.Subagents)
	})

	t.Run("v9 both sub_agents and subagents coexist", func(t *testing.T) {
		t.Parallel()

		yaml := `version: "9"
agents:
  root:
    model: openai/gpt-4o
    sub_agents:
      - legacy_helper
    subagents:
      - runtime_helper
  legacy_helper:
    model: openai/gpt-4o
  runtime_helper:
    model: openai/gpt-4o
`
		cfg, err := Load(t.Context(), NewBytesSource("test.yaml", []byte(yaml)))
		require.NoError(t, err)

		root, ok := cfg.Agents.Lookup("root")
		require.True(t, ok)

		assert.Equal(t, []string{"legacy_helper"}, root.SubAgents)
		require.Len(t, root.Subagents, 1)
		assert.Equal(t, "runtime_helper", root.Subagents[0].Agent)
	})

	t.Run("v8 sub_agents migrates to v9", func(t *testing.T) {
		t.Parallel()

		yaml := `version: "8"
agents:
  root:
    model: openai/gpt-4o
    sub_agents:
      - helper
  helper:
    model: openai/gpt-4o
`
		cfg, err := Load(t.Context(), NewBytesSource("test.yaml", []byte(yaml)))
		require.NoError(t, err)

		root, ok := cfg.Agents.Lookup("root")
		require.True(t, ok)

		// v8 sub_agents should survive migration to v9
		assert.Equal(t, []string{"helper"}, root.SubAgents)
		// subagents was not set in v8
		assert.Empty(t, root.Subagents)
	})
}

func TestV9_SubagentSpec_UnmarshalYAML(t *testing.T) {
	t.Parallel()

	t.Run("string shorthand", func(t *testing.T) {
		t.Parallel()

		var spec latest.SubagentSpec
		err := unmarshalYAML("researcher", &spec)
		require.NoError(t, err)
		assert.Equal(t, "researcher", spec.Agent)
		assert.Empty(t, spec.Description)
	})

	t.Run("full object form", func(t *testing.T) {
		t.Parallel()

		var spec latest.SubagentSpec
		err := unmarshalYAML(`agent: writer
description: "writes documents"`, &spec)
		require.NoError(t, err)
		assert.Equal(t, "writer", spec.Agent)
		assert.Equal(t, "writes documents", spec.Description)
	})
}

func TestV9_SubagentSpec_MarshalYAML(t *testing.T) {
	t.Parallel()

	t.Run("shorthand round-trip", func(t *testing.T) {
		t.Parallel()

		spec := latest.SubagentSpec{Agent: "researcher"}
		val, err := spec.MarshalYAML()
		require.NoError(t, err)
		// String shorthand
		assert.Equal(t, "researcher", val)
	})

	t.Run("full object round-trip", func(t *testing.T) {
		t.Parallel()

		spec := latest.SubagentSpec{Agent: "writer", Description: "writes documents"}
		val, err := spec.MarshalYAML()
		require.NoError(t, err)
		// Full object form
		m, ok := val.(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "writer", m["agent"])
		assert.Equal(t, "writes documents", m["description"])
	})
}

func TestValidateConfig_SubagentReferences(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cfg     *latest.Config
		wantErr string
	}{
		{
			name: "valid local subagent reference",
			cfg: &latest.Config{
				Agents: []latest.AgentConfig{
					{Name: "root", Model: "openai/gpt-4o", Subagents: []latest.SubagentSpec{{Agent: "helper"}}},
					{Name: "helper", Model: "openai/gpt-4o"},
				},
			},
		},
		{
			name: "non-existent local subagent reference",
			cfg: &latest.Config{
				Agents: []latest.AgentConfig{
					{Name: "root", Model: "openai/gpt-4o", Subagents: []latest.SubagentSpec{{Agent: "missing"}}},
				},
			},
			wantErr: "non-existent runtime subagent 'missing'",
		},
		{
			name: "external OCI subagent reference is allowed",
			cfg: &latest.Config{
				Agents: []latest.AgentConfig{
					{Name: "root", Model: "openai/gpt-4o", Subagents: []latest.SubagentSpec{{Agent: "agentcatalog/pirate"}}},
				},
			},
		},
		{
			name: "external subagent name collides with local agent",
			cfg: &latest.Config{
				Agents: []latest.AgentConfig{
					{Name: "root", Model: "openai/gpt-4o", Subagents: []latest.SubagentSpec{{Agent: "agentcatalog/pirate"}}},
					{Name: "pirate", Model: "openai/gpt-4o"},
				},
			},
			wantErr: "conflicts with a locally-defined agent",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := validateConfig(tt.cfg)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// unmarshalYAML is a test helper that unmarshals a YAML string into a value.
func unmarshalYAML(yamlStr string, v any) error {
	return yamlpkg.Unmarshal([]byte(yamlStr), v)
}
