package modelsgateway

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNormalizeIDs(t *testing.T) {
	t.Parallel()

	ids := []string{
		"anthropic/claude-sonnet-4-6", // prefixed: preserved
		"bare-model",                  // bare: routed through openai
		"dmr/ai/qwen3:latest",         // model part may contain slashes
		"anthropic/claude-sonnet-4-6", // duplicate: first wins
		"openai/bare-model",           // duplicate of normalized bare ID
		"openai/text-embedding-3",     // embedding by ID
		"",                            // invalid
		"foo/",                        // invalid: empty model
		"/bar",                        // invalid: empty provider
	}

	got := NormalizeIDs(t.Context(), ids, nil)

	assert.Equal(t, []Ref{
		{Provider: "anthropic", Model: "claude-sonnet-4-6"},
		{Provider: "openai", Model: "bare-model"},
		{Provider: "dmr", Model: "ai/qwen3:latest"},
	}, got, "order must deterministically follow the gateway's order")
}

func TestNormalizeIDs_ProviderCaseInsensitive(t *testing.T) {
	t.Parallel()

	got := NormalizeIDs(t.Context(), []string{
		"OpenAI/gpt-4o",      // mixed-case provider: lowercased
		"openai/gpt-4o",      // duplicate of the lowercased form
		"ANTHROPIC/Claude-X", // provider lowercased, model case preserved
	}, nil)

	assert.Equal(t, []Ref{
		{Provider: "openai", Model: "gpt-4o"},
		{Provider: "anthropic", Model: "Claude-X"},
	}, got, "mixed-case providers must normalize to the canonical lowercase form and deduplicate against it")
}

func TestNormalizeIDs_MetadataFilters(t *testing.T) {
	t.Parallel()

	metadata := map[string]Metadata{
		// The ID contains no "embed" substring; only the family says so.
		"google/vector-model": {Family: "text-embedding"},
		// Declared non-text output: not a chat model.
		"openai/image-only": {OutputModalities: []string{"image"}},
		// Declared text output: kept.
		"openai/gpt-4o": {OutputModalities: []string{"text"}},
		// No declared modalities: no signal, kept.
		"openai/no-modalities": {},
	}
	lookup := func(_ context.Context, prov, model string) (Metadata, bool) {
		m, ok := metadata[prov+"/"+model]
		return m, ok
	}

	got := NormalizeIDs(t.Context(), []string{
		"google/vector-model",
		"openai/image-only",
		"openai/gpt-4o",
		"openai/no-modalities",
		"openai/not-in-catalog",
	}, lookup)

	assert.Equal(t, []Ref{
		{Provider: "openai", Model: "gpt-4o"},
		{Provider: "openai", Model: "no-modalities"},
		{Provider: "openai", Model: "not-in-catalog"},
	}, got)
}
