package provider

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/docker/cagent/pkg/chat"
	"github.com/docker/cagent/pkg/config/latest"
	"github.com/docker/cagent/pkg/environment"
	"github.com/docker/cagent/pkg/model/provider/anthropic"
	"github.com/docker/cagent/pkg/model/provider/base"
	"github.com/docker/cagent/pkg/model/provider/dmr"
	"github.com/docker/cagent/pkg/model/provider/gemini"
	"github.com/docker/cagent/pkg/model/provider/openai"
	"github.com/docker/cagent/pkg/model/provider/options"
	"github.com/docker/cagent/pkg/model/provider/rulebased"
	"github.com/docker/cagent/pkg/rag/types"
	"github.com/docker/cagent/pkg/tools"
)

// Alias defines the configuration for a provider alias
type Alias struct {
	APIType     string // The actual API type to use (openai, anthropic, etc.)
	BaseURL     string // Default base URL for the provider
	TokenEnvVar string // Environment variable name for the API token
}

// Aliases maps provider names to their corresponding configurations
var Aliases = map[string]Alias{
	"requesty": {
		APIType:     "openai",
		BaseURL:     "https://router.requesty.ai/v1",
		TokenEnvVar: "REQUESTY_API_KEY",
	},
	"azure": {
		APIType:     "openai",
		TokenEnvVar: "AZURE_API_KEY",
	},
	"xai": {
		APIType:     "openai",
		BaseURL:     "https://api.x.ai/v1",
		TokenEnvVar: "XAI_API_KEY",
	},
	"nebius": {
		APIType:     "openai",
		BaseURL:     "https://api.studio.nebius.com/v1",
		TokenEnvVar: "NEBIUS_API_KEY",
	},
	"mistral": {
		APIType:     "openai",
		BaseURL:     "https://api.mistral.ai/v1",
		TokenEnvVar: "MISTRAL_API_KEY",
	},
	"ollama": {
		APIType: "openai",
		BaseURL: "http://localhost:11434/v1",
	},
}

// Provider defines the interface for model providers
type Provider interface {
	// ID returns the model provider ID
	ID() string
	// CreateChatCompletionStream creates a streaming chat completion request
	// It returns a stream that can be iterated over to get completion chunks
	CreateChatCompletionStream(
		ctx context.Context,
		messages []chat.Message,
		tools []tools.Tool,
	) (chat.MessageStream, error)
	// BaseConfig returns the base configuration of this provider
	BaseConfig() base.Config
}

// EmbeddingProvider defines the interface for providers that support embeddings.
type EmbeddingProvider interface {
	Provider
	// CreateEmbedding generates an embedding vector for the given text with usage tracking.
	CreateEmbedding(ctx context.Context, text string) (*base.EmbeddingResult, error)
}

// BatchEmbeddingProvider defines the interface for providers that support batch embeddings.
type BatchEmbeddingProvider interface {
	EmbeddingProvider
	// CreateBatchEmbedding generates embedding vectors for multiple texts with usage tracking.
	// Returns embeddings in the same order as input texts.
	CreateBatchEmbedding(ctx context.Context, texts []string) (*base.BatchEmbeddingResult, error)
}

// RerankingProvider defines the interface for providers that support reranking.
// Reranking models score query-document pairs to assess relevance.
type RerankingProvider interface {
	Provider
	// Rerank scores documents by relevance to the query.
	// Returns relevance scores in the same order as input documents.
	// Scores are typically in [0, 1] range where higher means more relevant.
	// criteria: Optional domain-specific guidance for relevance scoring (appended to base prompt)
	// documents: Array of types.Document with content and metadata
	Rerank(ctx context.Context, query string, documents []types.Document, criteria string) ([]float64, error)
}

// New creates a new provider from a model config.
// This is a convenience wrapper for NewWithModels with no models map.
func New(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (Provider, error) {
	return NewWithModels(ctx, cfg, nil, env, opts...)
}

// NewWithModels creates a new provider from a model config with access to the full models map.
// The models map is used to resolve model references in routing rules.
func NewWithModels(ctx context.Context, cfg *latest.ModelConfig, models map[string]latest.ModelConfig, env environment.Provider, opts ...options.Opt) (Provider, error) {
	slog.Debug("Creating model provider", "type", cfg.Provider, "model", cfg.Model)

	// Check if this model has routing rules - if so, create a rule-based router
	if len(cfg.Routing) > 0 {
		return createRuleBasedRouter(ctx, cfg, models, env, opts...)
	}

	return createDirectProvider(ctx, cfg, env, opts...)
}

// createRuleBasedRouter creates a rule-based routing provider.
func createRuleBasedRouter(ctx context.Context, cfg *latest.ModelConfig, models map[string]latest.ModelConfig, env environment.Provider, opts ...options.Opt) (Provider, error) {
	// Create a provider factory that can resolve model references
	factory := func(ctx context.Context, modelSpec string, models map[string]latest.ModelConfig, env environment.Provider, factoryOpts ...options.Opt) (rulebased.Provider, error) {
		// Check if modelSpec is a reference to a model in the models map
		if modelCfg, exists := models[modelSpec]; exists {
			// Prevent infinite recursion - referenced models cannot have routing rules
			if len(modelCfg.Routing) > 0 {
				return nil, fmt.Errorf("model %q has routing rules and cannot be used as a routing target", modelSpec)
			}
			p, err := createDirectProvider(ctx, &modelCfg, env, factoryOpts...)
			if err != nil {
				return nil, err
			}
			return p, nil
		}

		// Otherwise, treat as an inline model spec (e.g., "openai/gpt-4o")
		providerName, model, ok := strings.Cut(modelSpec, "/")
		if !ok {
			return nil, fmt.Errorf("invalid model spec %q: expected 'provider/model' format or a model reference", modelSpec)
		}

		inlineCfg := &latest.ModelConfig{
			Provider: providerName,
			Model:    model,
		}
		p, err := createDirectProvider(ctx, inlineCfg, env, factoryOpts...)
		if err != nil {
			return nil, err
		}
		return p, nil
	}

	return rulebased.NewClient(ctx, cfg, models, env, factory, opts...)
}

// createDirectProvider creates a provider without routing (direct model access).
func createDirectProvider(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (Provider, error) {
	// Apply provider alias defaults to the config
	enhancedCfg := applyProviderDefaults(cfg)

	// Resolve the provider type with priority:
	// 1. cfg.ProviderOpts["api_type"] (from custom provider or model override)
	// 2. built-in alias APIType
	// 3. provider name itself
	providerType := resolveProviderTypeFromConfig(enhancedCfg)

	switch providerType {
	case "openai", "openai_chatcompletions", "openai_responses":
		return openai.NewClient(ctx, enhancedCfg, env, opts...)

	case "anthropic":
		return anthropic.NewClient(ctx, enhancedCfg, env, opts...)

	case "google":
		return gemini.NewClient(ctx, enhancedCfg, env, opts...)

	case "dmr":
		return dmr.NewClient(ctx, enhancedCfg, opts...)

	default:
		slog.Error("Unknown provider type", "type", providerType)
		return nil, fmt.Errorf("unknown provider type: %s", providerType)
	}
}

// resolveProviderTypeFromConfig determines the provider type to use based on config.
// Priority:
// 1. cfg.ProviderOpts["api_type"] (from custom provider or model-level override)
// 2. built-in alias APIType (e.g., "mistral" -> "openai")
// 3. provider name itself (e.g., "openai", "anthropic")
func resolveProviderTypeFromConfig(cfg *latest.ModelConfig) string {
	// Check for api_type in ProviderOpts (set by custom providers or model override)
	if cfg.ProviderOpts != nil {
		if apiType, ok := cfg.ProviderOpts["api_type"].(string); ok && apiType != "" {
			slog.Debug("Using api_type from provider config",
				"provider", cfg.Provider,
				"model", cfg.Model,
				"api_type", apiType,
				"base_url", cfg.BaseURL,
			)
			return apiType
		}
	}

	// Check built-in alias
	if alias, exists := Aliases[cfg.Provider]; exists && alias.APIType != "" {
		return alias.APIType
	}

	// Fall back to provider name
	return cfg.Provider
}

// applyProviderDefaults applies default configuration from provider aliases to the model config
// This sets default base URLs and token keys if not already specified
func applyProviderDefaults(cfg *latest.ModelConfig) *latest.ModelConfig {
	// Create a copy to avoid modifying the original
	enhancedCfg := *cfg

	// Check if provider has alias configuration
	if alias, exists := Aliases[cfg.Provider]; exists {
		// Set default base URL if not already specified
		if enhancedCfg.BaseURL == "" && alias.BaseURL != "" {
			enhancedCfg.BaseURL = alias.BaseURL
		}

		// Set default token key if not already specified
		if enhancedCfg.TokenKey == "" && alias.TokenEnvVar != "" {
			enhancedCfg.TokenKey = alias.TokenEnvVar
		}
	}

	return &enhancedCfg
}
