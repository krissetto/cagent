package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/cagent/pkg/chat"
	"github.com/docker/cagent/pkg/config/latest"
	"github.com/docker/cagent/pkg/environment"
	"github.com/docker/cagent/pkg/tools"
)

// mockEnvProvider is a simple env provider for testing
type mockEnvProvider struct {
	values map[string]string
}

func (m *mockEnvProvider) Get(_ context.Context, name string) (string, bool) {
	v, ok := m.values[name]
	return v, ok
}

func newMockEnvProvider(values map[string]string) environment.Provider {
	return &mockEnvProvider{values: values}
}

// TestCustomProvider_RequestReachesServer tests that requests from a custom provider
// reach the configured server with the correct base URL, authorization, and token_key.
func TestCustomProvider_RequestReachesServer(t *testing.T) {
	t.Parallel()

	var (
		receivedAuth  string
		receivedPath  string
		receivedModel string
		mu            sync.Mutex
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		receivedAuth = r.Header.Get("Authorization")
		receivedPath = r.URL.Path
		mu.Unlock()

		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err == nil {
			if m, ok := payload["model"].(string); ok {
				mu.Lock()
				receivedModel = m
				mu.Unlock()
			}
		}

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)

		writeSSEChunk(w, map[string]any{
			"id": "test", "object": "chat.completion.chunk", "model": "gpt-4o",
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": "Hello"}, "finish_reason": nil}},
		})
		writeSSEChunk(w, map[string]any{
			"id": "test", "object": "chat.completion.chunk", "model": "gpt-4o",
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
		})
		writeSSEChunk(w, map[string]any{
			"id": "test", "object": "chat.completion.chunk", "model": "gpt-4o",
			"choices": []map[string]any{},
			"usage":   map[string]any{"prompt_tokens": 5, "completion_tokens": 1, "total_tokens": 6},
		})
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}))
	defer server.Close()

	// Custom token key to verify token_key feature
	customTokenKey := "MY_CUSTOM_GATEWAY_TOKEN"
	expectedToken := "test-secret-key-123"

	modelCfg := &latest.ModelConfig{
		Provider: "my_custom_provider",
		Model:    "gpt-4o",
		BaseURL:  server.URL,
		TokenKey: customTokenKey,
		ProviderOpts: map[string]any{
			"api_type": "openai_chatcompletions",
		},
	}

	env := newMockEnvProvider(map[string]string{
		customTokenKey: expectedToken,
	})

	provider, err := New(t.Context(), modelCfg, env)
	require.NoError(t, err)

	stream, err := provider.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "Hello"}}, []tools.Tool{})
	require.NoError(t, err)
	defer stream.Close()

	drainStream(t, stream)

	mu.Lock()
	defer mu.Unlock()

	assert.Equal(t, "Bearer "+expectedToken, receivedAuth, "Token from custom env var should be used")
	assert.Equal(t, "/chat/completions", receivedPath, "Request should go to /chat/completions")
	assert.Equal(t, "gpt-4o", receivedModel, "Model should be passed in the request")
}

// TestCustomProvider_ResponsesAPIType tests that api_type=openai_responses routes to /responses
func TestCustomProvider_ResponsesAPIType(t *testing.T) {
	t.Parallel()

	var (
		receivedPath string
		mu           sync.Mutex
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		receivedPath = r.URL.Path
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)

		// Responses API format
		events := []map[string]any{
			{"type": "response.created", "response_id": "resp_test"},
			{"type": "response.output_item.added", "item": map[string]any{"type": "message", "role": "assistant"}},
			{"type": "response.output_text.delta", "delta": map[string]any{"text": "Hello"}},
			{"type": "response.done", "status": "completed"},
		}
		for _, event := range events {
			eventJSON, _ := json.Marshal(event)
			_, _ = w.Write([]byte("event: " + event["type"].(string) + "\ndata: " + string(eventJSON) + "\n\n"))
			flusher.Flush()
		}
	}))
	defer server.Close()

	modelCfg := &latest.ModelConfig{
		Provider: "responses_provider",
		Model:    "gpt-4o-pro",
		BaseURL:  server.URL,
		TokenKey: "API_KEY",
		ProviderOpts: map[string]any{
			"api_type": "openai_responses",
		},
	}

	env := newMockEnvProvider(map[string]string{"API_KEY": "test"})

	provider, err := New(t.Context(), modelCfg, env)
	require.NoError(t, err)

	stream, err := provider.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "Hello"}}, []tools.Tool{})
	require.NoError(t, err)
	defer stream.Close()

	// Drain (may error due to mock format, but we only care about the path)
	for {
		if _, err := stream.Recv(); err != nil {
			break
		}
	}

	mu.Lock()
	defer mu.Unlock()

	assert.Equal(t, "/responses", receivedPath, "api_type=openai_responses should route to /responses")
}

// TestCustomProvider_ChatCompletionsAPIType tests that api_type=openai_chatcompletions
// forces Chat Completions even for models that would normally use Responses API
func TestCustomProvider_ChatCompletionsAPIType(t *testing.T) {
	t.Parallel()

	var (
		receivedPath string
		mu           sync.Mutex
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		receivedPath = r.URL.Path
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)

		writeSSEChunk(w, map[string]any{
			"id": "test", "object": "chat.completion.chunk", "model": "gpt-4o-pro",
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": "Hello"}, "finish_reason": "stop"}},
		})
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}))
	defer server.Close()

	// Model name with "-pro" suffix would normally trigger Responses API
	modelCfg := &latest.ModelConfig{
		Provider: "openai",
		Model:    "gpt-4o-pro",
		BaseURL:  server.URL,
		TokenKey: "OPENAI_API_KEY",
		ProviderOpts: map[string]any{
			"api_type": "openai_chatcompletions", // Force Chat Completions
		},
	}

	env := newMockEnvProvider(map[string]string{"OPENAI_API_KEY": "test"})

	provider, err := New(t.Context(), modelCfg, env)
	require.NoError(t, err)

	stream, err := provider.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "Test"}}, []tools.Tool{})
	require.NoError(t, err)
	defer stream.Close()

	for {
		if _, err := stream.Recv(); err != nil {
			break
		}
	}

	mu.Lock()
	defer mu.Unlock()

	assert.Equal(t, "/chat/completions", receivedPath, "api_type=openai_chatcompletions should force Chat Completions")
}

// TestCustomProvider_MissingAPIKey tests error handling for missing API key
func TestCustomProvider_MissingAPIKey(t *testing.T) {
	t.Parallel()

	modelCfg := &latest.ModelConfig{
		Provider: "custom_provider",
		Model:    "test-model",
		BaseURL:  "http://localhost:8888",
		TokenKey: "MISSING_API_KEY",
		ProviderOpts: map[string]any{
			"api_type": "openai_chatcompletions",
		},
	}

	env := newMockEnvProvider(map[string]string{}) // Empty - key not set

	_, err := New(t.Context(), modelCfg, env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MISSING_API_KEY", "Error should mention the missing env var")
}

// TestResolveProviderTypeFromConfig tests the provider type resolution logic
func TestResolveProviderTypeFromConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		config   *latest.ModelConfig
		expected string
	}{
		{
			name: "api_type from ProviderOpts takes priority",
			config: &latest.ModelConfig{
				Provider:     "openai",
				ProviderOpts: map[string]any{"api_type": "openai_responses"},
			},
			expected: "openai_responses",
		},
		{
			name: "built-in alias takes second priority",
			config: &latest.ModelConfig{
				Provider: "mistral", // Has alias with APIType: "openai"
			},
			expected: "openai",
		},
		{
			name: "provider name is fallback",
			config: &latest.ModelConfig{
				Provider: "anthropic",
			},
			expected: "anthropic",
		},
		{
			name: "custom provider with api_type",
			config: &latest.ModelConfig{
				Provider:     "my_custom_provider",
				ProviderOpts: map[string]any{"api_type": "openai_chatcompletions"},
			},
			expected: "openai_chatcompletions",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.expected, resolveProviderTypeFromConfig(tt.config))
		})
	}
}

// Helper functions

func writeSSEChunk(w http.ResponseWriter, data map[string]any) {
	jsonData, _ := json.Marshal(data)
	_, _ = w.Write([]byte("data: " + string(jsonData) + "\n\n"))
}

func drainStream(t *testing.T, stream chat.MessageStream) {
	t.Helper()
	for {
		_, err := stream.Recv()
		if err == io.EOF {
			return
		}
		if err != nil {
			return
		}
	}
}
