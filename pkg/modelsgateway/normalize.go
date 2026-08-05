package modelsgateway

import (
	"context"
	"slices"
	"strings"
)

// Ref is a normalized provider/model reference served by a models gateway.
// It is deliberately a small neutral type so both the runtime (ModelChoice)
// and the CLI (modelRow) can adapt it without depending on each other.
type Ref struct {
	Provider string
	Model    string
}

// String renders the reference in the "provider/model" form used across
// the codebase.
func (r Ref) String() string { return r.Provider + "/" + r.Model }

// Metadata is the subset of catalog information NormalizeIDs consults to
// filter out models unsuitable for chat.
type Metadata struct {
	// Family is the model family from the catalog (e.g. "text-embedding").
	Family string
	// OutputModalities lists the output modalities the catalog declares.
	OutputModalities []string
}

// MetadataFunc looks up catalog metadata for a provider/model pair. It
// returns ok=false when the model is unknown to the catalog, in which case
// NormalizeIDs keeps the model (no signal to exclude it). Callers typically
// back it with a models.dev store; a nil MetadataFunc skips metadata-based
// filtering entirely.
type MetadataFunc func(ctx context.Context, provider, model string) (Metadata, bool)

// NormalizeIDs transforms the raw model IDs returned by a gateway's
// /v1/models endpoint into provider/model references:
//
//   - IDs already prefixed with a provider ("anthropic/claude-x") keep it;
//     bare IDs are routed through the openai provider, matching how the
//     gateway serves them over its OpenAI-compatible endpoint.
//   - Providers are lowercased ("OpenAI/gpt-4o" → "openai/gpt-4o") so a
//     gateway's casing matches the canonical provider IDs used for catalog
//     lookups and deduplication; the model part keeps its case.
//   - Malformed references (empty provider or model) are dropped.
//   - Embedding models are excluded, identified by the model ID or by the
//     catalog family when metadata is available.
//   - Models whose catalog metadata declares output modalities without
//     "text" are excluded (not suitable for chat); models without metadata
//     or without declared modalities are kept.
//   - Duplicates are removed; the first occurrence wins, so the result
//     order deterministically follows the gateway's order.
func NormalizeIDs(ctx context.Context, ids []string, lookup MetadataFunc) []Ref {
	seen := make(map[string]bool, len(ids))
	refs := make([]Ref, 0, len(ids))

	for _, id := range ids {
		prov, model, ok := strings.Cut(id, "/")
		if !ok {
			prov, model = "openai", id
		}
		prov = strings.ToLower(prov)
		if prov == "" || model == "" {
			continue
		}

		// Resolve catalog metadata before the embedding filter so the
		// catalog family (e.g. "text-embedding") is consulted even when
		// the model ID itself doesn't contain "embed".
		var meta Metadata
		var found bool
		if lookup != nil {
			meta, found = lookup(ctx, prov, model)
		}
		if isEmbeddingModel(meta.Family, model) {
			continue
		}
		if found && len(meta.OutputModalities) > 0 && !slices.Contains(meta.OutputModalities, "text") {
			continue
		}

		ref := prov + "/" + model
		if seen[ref] {
			continue
		}
		seen[ref] = true
		refs = append(refs, Ref{Provider: prov, Model: model})
	}

	return refs
}

// isEmbeddingModel reports whether the family or model name identifies an
// embedding model, which is never suitable for chat.
func isEmbeddingModel(family, name string) bool {
	return strings.Contains(strings.ToLower(family), "embed") ||
		strings.Contains(strings.ToLower(name), "embed")
}
