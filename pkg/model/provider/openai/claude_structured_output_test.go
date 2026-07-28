package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/tools"
)

const judgePrompt = "Evaluate whether the response satisfies the criteria and respond with your judgment."

// judgeStructuredOutput mirrors the eval judge's structured-output
// configuration (pkg/evaluation), the setup that triggered issue #3840.
func judgeStructuredOutput() *latest.StructuredOutput {
	return &latest.StructuredOutput{
		Name:        "judge_response",
		Description: "Evaluation result for a relevance criterion",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"result": map[string]any{"type": "string", "enum": []string{"pass", "fail"}},
				"reason": map[string]any{"type": "string"},
			},
			"required":             []string{"result", "reason"},
			"additionalProperties": false,
		},
		Strict: true,
	}
}

// structuredOutputChatRequest drives a Chat Completions request carrying the
// judge prompt with structured output configured against a mock server and
// returns the decoded request body.
func structuredOutputChatRequest(t *testing.T, cfg *latest.ModelConfig, env map[string]string) map[string]any {
	t.Helper()
	return structuredOutputChatRequestWith(t, cfg, env,
		[]chat.Message{{Role: chat.MessageRoleUser, Content: judgePrompt}}, nil)
}

// structuredOutputChatRequestWith is structuredOutputChatRequest's
// fully-parameterized sibling: callers supply the message history and tools.
func structuredOutputChatRequestWith(t *testing.T, cfg *latest.ModelConfig, env map[string]string, messages []chat.Message, requestTools []tools.Tool) map[string]any {
	t.Helper()

	server, body := captureRequestBody(t)
	cfg.BaseURL = server.URL

	client, err := NewClient(t.Context(), cfg, environment.NewMapEnvProvider(env),
		options.WithStructuredOutput(judgeStructuredOutput()))
	require.NoError(t, err)

	stream, err := client.CreateChatCompletionStream(t.Context(), messages, requestTools)
	require.NoError(t, err)
	defer stream.Close()
	drainReasoningTestStream(t, stream)

	var req map[string]any
	require.NoError(t, json.Unmarshal(body(), &req))
	return req
}

// assertSchemaInstructionContent asserts content carries the serialized
// structured-output schema, its name and description, and the raw-JSON-only
// directives.
func assertSchemaInstructionContent(t *testing.T, content string) {
	t.Helper()

	schemaJSON, err := json.Marshal(judgeStructuredOutput().Schema)
	require.NoError(t, err)
	assert.Contains(t, content, string(schemaJSON), "instruction must embed the serialized JSON schema")
	assert.Contains(t, content, "Schema name: judge_response", "instruction must carry the schema name")
	assert.Contains(t, content, "Schema description: Evaluation result for a relevance criterion",
		"instruction must carry the schema description")
	assert.Contains(t, content, "no Markdown formatting")
	assert.Contains(t, content, "no code fences")
	assert.Contains(t, content, "no text before or after the JSON object")
}

// assertSchemaInstructionMessage asserts msg is a system message carrying the
// schema instruction.
func assertSchemaInstructionMessage(t *testing.T, msg map[string]any) {
	t.Helper()

	assert.Equal(t, "system", msg["role"])
	content, ok := msg["content"].(string)
	require.True(t, ok, "instruction content must be a plain string")
	assertSchemaInstructionContent(t, content)
}

// TestChatCompletions_CopilotClaudeStructuredOutputInstruction verifies that
// a Claude model served through GitHub Copilot's OpenAI-compatible endpoint
// receives both the native response_format and the prompt-level schema
// instruction fallback, prepended as a system message so system content
// stays at the beginning of the conversation. Copilot/Claude may ignore
// response_format.json_schema and answer with Markdown prose, breaking
// strict JSON consumers such as the eval judge — see
// https://github.com/docker/docker-agent/issues/3840.
func TestChatCompletions_CopilotClaudeStructuredOutputInstruction(t *testing.T) {
	t.Parallel()

	req := structuredOutputChatRequest(t,
		&latest.ModelConfig{
			Provider: "github-copilot",
			Model:    "claude-opus-4.8",
			TokenKey: "GITHUB_TOKEN",
		},
		map[string]string{"GITHUB_TOKEN": "ghp_secret"},
	)

	messages, ok := req["messages"].([]any)
	require.True(t, ok)
	require.Len(t, messages, 2)

	first, ok := messages[0].(map[string]any)
	require.True(t, ok)
	assertSchemaInstructionMessage(t, first)

	last, ok := messages[1].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "user", last["role"])
	assert.Equal(t, judgePrompt, last["content"], "the original prompt must be preserved")

	// response_format stays on the request as best effort.
	responseFormat, ok := req["response_format"].(map[string]any)
	require.True(t, ok, "response_format must still be sent")
	assert.Equal(t, "json_schema", responseFormat["type"])
}

// TestChatCompletions_CustomProviderClaudeStructuredOutputInstruction
// verifies the fallback also applies to Claude reached through a custom
// OpenAI-compatible provider (LiteLLM-style), where model detection relies
// on the claude- name prefix.
func TestChatCompletions_CustomProviderClaudeStructuredOutputInstruction(t *testing.T) {
	t.Parallel()

	req := structuredOutputChatRequest(t,
		&latest.ModelConfig{
			Provider: "litellm",
			Model:    "claude-sonnet-4-5",
			TokenKey: "MY_TOKEN",
			ProviderOpts: map[string]any{
				"api_type": "openai_chatcompletions",
			},
		},
		map[string]string{"MY_TOKEN": "secret"},
	)

	messages, ok := req["messages"].([]any)
	require.True(t, ok)
	require.Len(t, messages, 2)

	first, ok := messages[0].(map[string]any)
	require.True(t, ok)
	assertSchemaInstructionMessage(t, first)
}

// TestChatCompletions_DockerGatewayClaudeStructuredOutputInstruction drives
// the real options.WithGateway wire path: the gateway must receive the
// request on the OpenAI-compatible path with cagent's identity headers, and
// the request body must carry the schema instruction fallback.
func TestChatCompletions_DockerGatewayClaudeStructuredOutputInstruction(t *testing.T) {
	t.Parallel()

	var (
		mu     sync.Mutex
		body   []byte
		header http.Header
		path   string
	)
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		body = b
		header = r.Header.Clone()
		path = r.URL.Path
		mu.Unlock()
		writeSSEResponse(w)
	}))
	t.Cleanup(gateway.Close)

	cfg := &latest.ModelConfig{
		Provider: "github-copilot",
		Model:    "claude-opus-4.8",
		// The provider's public endpoint the gateway forwards to; requests
		// themselves go to the gateway URL.
		BaseURL: "https://api.githubcopilot.com",
	}
	// gateway.URL is 127.0.0.1, which IsTrustedDockerURL considers trusted,
	// so the Docker Desktop token must be supplied.
	env := environment.NewMapEnvProvider(map[string]string{
		environment.DockerDesktopTokenEnv: "test-dd-token",
	})

	client, err := NewClient(t.Context(), cfg, env,
		options.WithGateway(gateway.URL),
		options.WithStructuredOutput(judgeStructuredOutput()))
	require.NoError(t, err)

	stream, err := client.CreateChatCompletionStream(
		t.Context(),
		[]chat.Message{{Role: chat.MessageRoleUser, Content: judgePrompt}},
		nil,
	)
	require.NoError(t, err)
	defer stream.Close()
	drainReasoningTestStream(t, stream)

	mu.Lock()
	defer mu.Unlock()

	assert.Equal(t, "/v1/chat/completions", path, "the gateway must receive the request on the OpenAI-compatible path")
	assert.Equal(t, "github-copilot", header.Get("X-Cagent-Provider"))
	assert.Equal(t, "claude-opus-4.8", header.Get("X-Cagent-Model"))
	assert.Equal(t, "https://api.githubcopilot.com", header.Get("X-Cagent-Forward"))
	assert.Equal(t, "Bearer test-dd-token", header.Get("Authorization"))

	var req map[string]any
	require.NoError(t, json.Unmarshal(body, &req))

	messages, ok := req["messages"].([]any)
	require.True(t, ok)
	require.Len(t, messages, 2)

	first, ok := messages[0].(map[string]any)
	require.True(t, ok)
	assertSchemaInstructionMessage(t, first)

	last, ok := messages[1].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "user", last["role"])
	assert.Equal(t, judgePrompt, last["content"])
}

// TestChatCompletions_ClaudeStructuredOutputMixedHistory verifies the
// fallback keeps a realistic mixed-role history intact: the instruction is
// merged into the existing leading system message (system content must stay
// at the beginning; some backends only accept a system message at index 0)
// and every original message survives in order.
func TestChatCompletions_ClaudeStructuredOutputMixedHistory(t *testing.T) {
	t.Parallel()

	const agentInstructions = "You are an evaluation judge."
	history := []chat.Message{
		{Role: chat.MessageRoleSystem, Content: agentInstructions},
		{Role: chat.MessageRoleUser, Content: "Evaluate the session transcript."},
		{Role: chat.MessageRoleAssistant, ToolCalls: []tools.ToolCall{{
			ID:       "call_1",
			Type:     "function",
			Function: tools.FunctionCall{Name: "read_transcript", Arguments: "{}"},
		}}},
		{Role: chat.MessageRoleTool, ToolCallID: "call_1", Content: "transcript contents"},
		{Role: chat.MessageRoleUser, Content: judgePrompt},
	}

	req := structuredOutputChatRequestWith(t,
		&latest.ModelConfig{
			Provider: "github-copilot",
			Model:    "claude-opus-4.8",
			TokenKey: "GITHUB_TOKEN",
		},
		map[string]string{"GITHUB_TOKEN": "ghp_secret"},
		history,
		nil,
	)

	messages, ok := req["messages"].([]any)
	require.True(t, ok)
	require.Len(t, messages, len(history), "no message may be added, dropped, or reordered")

	first, ok := messages[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "system", first["role"], "system content must stay at the beginning")
	content, ok := first["content"].(string)
	require.True(t, ok)
	assert.True(t, strings.HasPrefix(content, agentInstructions), "the agent instructions must stay first")
	assertSchemaInstructionContent(t, content)

	wantRoles := []string{"user", "assistant", "tool", "user"}
	for i, want := range wantRoles {
		msg, ok := messages[i+1].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, want, msg["role"], "message %d must keep its role", i+1)
	}

	second, _ := messages[1].(map[string]any)
	assert.Equal(t, "Evaluate the session transcript.", second["content"])

	third, _ := messages[2].(map[string]any)
	toolCalls, ok := third["tool_calls"].([]any)
	require.True(t, ok, "the assistant tool call must be preserved")
	require.Len(t, toolCalls, 1)

	fourth, _ := messages[3].(map[string]any)
	assert.Equal(t, "call_1", fourth["tool_call_id"])
	assert.Equal(t, "transcript contents", fourth["content"])

	fifth, _ := messages[4].(map[string]any)
	assert.Equal(t, judgePrompt, fifth["content"])
}

// TestChatCompletions_ClaudeStructuredOutputKeepsTools verifies that the
// schema instruction injection leaves the request's tools and tool_choice
// untouched: structured output and tool calling must coexist.
func TestChatCompletions_ClaudeStructuredOutputKeepsTools(t *testing.T) {
	t.Parallel()

	req := structuredOutputChatRequestWith(t,
		&latest.ModelConfig{
			Provider: "github-copilot",
			Model:    "claude-opus-4.8",
			TokenKey: "GITHUB_TOKEN",
		},
		map[string]string{"GITHUB_TOKEN": "ghp_secret"},
		[]chat.Message{{Role: chat.MessageRoleUser, Content: judgePrompt}},
		[]tools.Tool{{
			Name:        "search",
			Description: "Search the web",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"q": map[string]any{"type": "string"},
				},
			},
		}},
	)

	toolsParam, ok := req["tools"].([]any)
	require.True(t, ok, "tools must stay on the request")
	require.Len(t, toolsParam, 1)
	tool, ok := toolsParam[0].(map[string]any)
	require.True(t, ok)
	fn, ok := tool["function"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "search", fn["name"])

	assert.Equal(t, "auto", req["tool_choice"], "tool_choice must stay on the request")

	messages, ok := req["messages"].([]any)
	require.True(t, ok)
	require.Len(t, messages, 2)
	first, ok := messages[0].(map[string]any)
	require.True(t, ok)
	assertSchemaInstructionMessage(t, first)

	responseFormat, ok := req["response_format"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "json_schema", responseFormat["type"])
}

// TestChatCompletions_NonClaudeStructuredOutputUnchanged verifies that an
// ordinary OpenAI model keeps its original prompt: no fallback instruction is
// injected, only the native response_format is sent.
func TestChatCompletions_NonClaudeStructuredOutputUnchanged(t *testing.T) {
	t.Parallel()

	req := structuredOutputChatRequest(t,
		&latest.ModelConfig{
			Provider: "openai",
			Model:    "gpt-4o",
			TokenKey: "MY_TOKEN",
		},
		map[string]string{"MY_TOKEN": "secret"},
	)

	messages, ok := req["messages"].([]any)
	require.True(t, ok)
	require.Len(t, messages, 1, "no fallback message may be injected for non-Claude models")

	first, ok := messages[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "user", first["role"])
	assert.Equal(t, judgePrompt, first["content"])

	responseFormat, ok := req["response_format"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "json_schema", responseFormat["type"])
}

// TestResponses_ClaudeStructuredOutputInstruction verifies the fallback on
// the Responses API path, reachable for Claude via an explicit api_type.
func TestResponses_ClaudeStructuredOutputInstruction(t *testing.T) {
	t.Parallel()

	server, body := captureResponsesRequestBody(t)
	cfg := &latest.ModelConfig{
		Provider: "litellm",
		Model:    "claude-opus-4.8",
		BaseURL:  server.URL,
		TokenKey: "MY_TOKEN",
		ProviderOpts: map[string]any{
			"api_type": "openai_responses",
		},
	}

	client, err := NewClient(t.Context(), cfg, environment.NewMapEnvProvider(map[string]string{"MY_TOKEN": "secret"}),
		options.WithStructuredOutput(judgeStructuredOutput()))
	require.NoError(t, err)

	stream, err := client.CreateChatCompletionStream(
		t.Context(),
		[]chat.Message{{Role: chat.MessageRoleUser, Content: judgePrompt}},
		nil,
	)
	require.NoError(t, err)
	defer stream.Close()
	drainReasoningTestStream(t, stream)

	var req map[string]any
	require.NoError(t, json.Unmarshal(body(), &req))

	input, ok := req["input"].([]any)
	require.True(t, ok)
	require.Len(t, input, 2)

	first, ok := input[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "system", first["role"], "the instruction must lead the input")

	parts, ok := first["content"].([]any)
	require.True(t, ok)
	require.Len(t, parts, 1)
	part, ok := parts[0].(map[string]any)
	require.True(t, ok)
	text, _ := part["text"].(string)
	assertSchemaInstructionContent(t, text)

	last, ok := input[1].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "user", last["role"])
	assert.Equal(t, judgePrompt, last["content"], "the original prompt must be preserved")

	// The native structured-output format stays on the request.
	textCfg, ok := req["text"].(map[string]any)
	require.True(t, ok)
	format, ok := textCfg["format"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "json_schema", format["type"])
}

// TestClaudeSchemaInstruction_OmitsEmptyNameAndDescription verifies the
// instruction only carries the name/description lines when they are set.
func TestClaudeSchemaInstruction_OmitsEmptyNameAndDescription(t *testing.T) {
	t.Parallel()

	full, err := claudeSchemaInstruction(judgeStructuredOutput())
	require.NoError(t, err)
	assertSchemaInstructionContent(t, full)

	minimal, err := claudeSchemaInstruction(&latest.StructuredOutput{
		Schema: map[string]any{"type": "object"},
	})
	require.NoError(t, err)
	assert.NotContains(t, minimal, "Schema name:")
	assert.NotContains(t, minimal, "Schema description:")
	assert.Contains(t, minimal, `{"type":"object"}`)
	assert.Contains(t, minimal, "no code fences")
}

// TestWithClaudeSchemaInstruction_DoesNotMutateCallerMessages exercises the
// helper directly: without a leading system message the instruction is
// prepended, and the caller-owned slice (including spare backing-array
// capacity) must stay untouched.
func TestWithClaudeSchemaInstruction_DoesNotMutateCallerMessages(t *testing.T) {
	t.Parallel()

	client := &Client{Config: base.Config{
		ModelConfig:  latest.ModelConfig{Provider: "github-copilot", Model: "claude-opus-4.8"},
		ModelOptions: options.Apply(options.WithStructuredOutput(judgeStructuredOutput())),
	}}

	messages := make([]chat.Message, 1, 4)
	messages[0] = chat.Message{Role: chat.MessageRoleUser, Content: judgePrompt}

	out := client.withClaudeSchemaInstruction(t.Context(), messages)

	require.Len(t, out, 2)
	assert.Equal(t, chat.MessageRoleSystem, out[0].Role)
	assert.Contains(t, out[0].Content, "no code fences")
	assert.Equal(t, judgePrompt, out[1].Content)

	require.Len(t, messages, 1)
	assert.Equal(t, chat.Message{Role: chat.MessageRoleUser, Content: judgePrompt}, messages[0])
	assert.Equal(t, chat.Message{}, messages[:2][1], "the caller's backing array must not be written to")
}

// TestWithClaudeSchemaInstruction_MergesIntoLeadingSystemMessage verifies the
// merge branch: with an existing leading system message the instruction is
// folded into a copy of it (no extra system message) and the caller's
// message stays untouched.
func TestWithClaudeSchemaInstruction_MergesIntoLeadingSystemMessage(t *testing.T) {
	t.Parallel()

	client := &Client{Config: base.Config{
		ModelConfig:  latest.ModelConfig{Provider: "github-copilot", Model: "claude-opus-4.8"},
		ModelOptions: options.Apply(options.WithStructuredOutput(judgeStructuredOutput())),
	}}

	const agentInstructions = "You are an evaluation judge."
	messages := []chat.Message{
		{Role: chat.MessageRoleSystem, Content: agentInstructions},
		{Role: chat.MessageRoleUser, Content: judgePrompt},
	}

	out := client.withClaudeSchemaInstruction(t.Context(), messages)

	require.Len(t, out, 2)
	assert.Equal(t, chat.MessageRoleSystem, out[0].Role)
	assert.True(t, strings.HasPrefix(out[0].Content, agentInstructions), "the agent instructions must stay first")
	assert.Contains(t, out[0].Content, "no code fences")
	assert.Equal(t, judgePrompt, out[1].Content)

	assert.Equal(t, agentInstructions, messages[0].Content, "the caller's message must not be mutated")
}

// TestWithClaudeSchemaInstruction_NoOpWithoutStructuredOutput verifies the
// helper leaves messages alone when structured output is not configured,
// even for Claude models.
func TestWithClaudeSchemaInstruction_NoOpWithoutStructuredOutput(t *testing.T) {
	t.Parallel()

	client := &Client{Config: base.Config{
		ModelConfig: latest.ModelConfig{Provider: "github-copilot", Model: "claude-opus-4.8"},
	}}

	messages := []chat.Message{{Role: chat.MessageRoleUser, Content: judgePrompt}}
	out := client.withClaudeSchemaInstruction(t.Context(), messages)

	require.Len(t, out, 1)
	assert.Equal(t, judgePrompt, out[0].Content)
}

// TestWithClaudeSchemaInstruction_NeverFetchesModelsDevCatalog is the
// regression test for the request hot path: Claude detection for the schema
// instruction must rely on modelinfo's name-pattern fallback alone and never
// consult the configured models.dev store, whose cold cache would otherwise
// trigger a catalog fetch on every structured-output request.
func TestWithClaudeSchemaInstruction_NeverFetchesModelsDevCatalog(t *testing.T) {
	t.Parallel()

	fetches := 0
	store, err := modelsdev.NewStore(
		// A cache path that does not exist: any store lookup would have to
		// fetch the catalog and be counted.
		modelsdev.WithCache(filepath.Join(t.TempDir(), "models_dev.json")),
		modelsdev.WithFetcher(func(context.Context, string) (*modelsdev.Database, string, error) {
			fetches++
			return nil, "", errors.New("models.dev must not be fetched on the request hot path")
		}),
	)
	require.NoError(t, err)

	client := &Client{Config: base.Config{
		ModelConfig: latest.ModelConfig{Provider: "github-copilot", Model: "claude-opus-4.8"},
		ModelOptions: options.Apply(
			options.WithStructuredOutput(judgeStructuredOutput()),
			options.WithModelsDevStore(store),
		),
	}}

	out := client.withClaudeSchemaInstruction(t.Context(),
		[]chat.Message{{Role: chat.MessageRoleUser, Content: judgePrompt}})

	require.Len(t, out, 2)
	assert.Equal(t, chat.MessageRoleSystem, out[0].Role)
	assertSchemaInstructionContent(t, out[0].Content)
	assert.Equal(t, judgePrompt, out[1].Content)

	assert.Zero(t, fetches, "Claude detection must not fetch the models.dev catalog")
}
