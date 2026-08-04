package config

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
)

// TestGatherEnvVarsForModels_TemplatedAliasBaseURL verifies that the env vars
// referenced by an alias's templated base URL (e.g. Cloudflare's
// account/gateway-scoped endpoints) are reported as required, alongside the
// token, so a missing account/gateway id is caught by the preflight check
// instead of only failing at provider-build time.
func TestGatherEnvVarsForModels_TemplatedAliasBaseURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		provider    string
		wantPresent []string
	}{
		{
			name:        "workers-ai requires account id and token",
			provider:    "cloudflare-workers-ai",
			wantPresent: []string{"CLOUDFLARE_API_TOKEN", "CLOUDFLARE_ACCOUNT_ID"},
		},
		{
			name:        "ai-gateway requires account id, gateway id and token",
			provider:    "cloudflare-ai-gateway",
			wantPresent: []string{"CLOUDFLARE_API_TOKEN", "CLOUDFLARE_ACCOUNT_ID", "CLOUDFLARE_GATEWAY_ID"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := &latest.Config{
				Agents: []latest.AgentConfig{{Name: "a", Model: "m"}},
				Models: map[string]latest.ModelConfig{
					"m": {Provider: tt.provider, Model: "some-model"},
				},
			}

			got := GatherEnvVarsForModels(t.Context(), cfg, environment.NewNoEnvProvider())
			for _, want := range tt.wantPresent {
				assert.Contains(t, got, want, "expected %q in %v", want, got)
			}
		})
	}
}

func TestGatherEnvVarsForModels_GitHubCopilotAcceptsGHToken(t *testing.T) {
	t.Parallel()

	cfg := &latest.Config{
		Agents: []latest.AgentConfig{{Name: "a", Model: "m"}},
		Models: map[string]latest.ModelConfig{
			"m": {Provider: "github-copilot", Model: "gpt-4.1"},
		},
	}

	got := GatherEnvVarsForModels(t.Context(), cfg, environment.NewMapEnvProvider(map[string]string{
		"GH_TOKEN": "gh-token",
	}))

	assert.Contains(t, got, "GH_TOKEN")
	assert.NotContains(t, got, "GITHUB_TOKEN")
}
