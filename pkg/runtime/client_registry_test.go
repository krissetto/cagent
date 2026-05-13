package runtime

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEventRegistryCompleteness ensures that every event type emitted by the
// runtime has a corresponding decoder entry in the HTTP client's SSE event
// registry. When a new *Event struct is added to event.go, its type string
// must be added to this list AND to NewClient's registry map in client.go.
//
// This test is the compile-time-adjacent safety net mandated by the bg-agents
// implementation plan so that SSE regressions (missing decoder → silently
// dropped events) never recur.
func TestEventRegistryCompleteness(t *testing.T) {
	t.Parallel()

	client, err := NewClient("http://localhost")
	require.NoError(t, err)

	// Canonical list of all event type strings produced by constructors in event.go.
	// Sorted alphabetically for readability.
	allEventTypes := []string{
		"agent_choice",
		"agent_choice_reasoning",
		"agent_info",
		"agent_switching",
		"authorization_event",
		"connection_lost",
		"connection_restored",
		"elicitation_request",
		"error",
		"hook_blocked",
		"hook_finished",
		"hook_started",
		"live_session_tree_changed",
		"max_iterations_reached",
		"mcp_init_finished",
		"mcp_init_started",
		"message_added",
		"model_fallback",
		"parent_idle",
		"parent_resume",
		"partial_tool_call",
		"rag_indexing_completed",
		"rag_indexing_progress",
		"rag_indexing_started",
		"session_compaction",
		"session_summary",
		"session_title",
		"shell",
		"stream_started",
		"stream_stopped",
		"sub_session_completed",
		"subagent_sent",
		"subagent_started",
		"subagent_update",
		"team_info",
		"token_usage",
		"tool_call",
		"tool_call_confirmation",
		"tool_call_response",
		"toolset_info",
		"turn_ended",
		"turn_started",
		"user_message",
		"warning",
	}

	for _, typ := range allEventTypes {
		t.Run(typ, func(t *testing.T) {
			factory, ok := client.registry[typ]
			assert.Truef(t, ok, "event type %q is emitted by event.go but missing from the client SSE registry in NewClient", typ)
			if ok {
				// Verify the factory produces a non-nil Event.
				ev := factory()
				assert.NotNilf(t, ev, "registry factory for %q returned nil", typ)
			}
		})
	}

	// Reverse check: no stale registry entries for types that no longer exist.
	for registeredType := range client.registry {
		assert.Truef(t, slices.Contains(allEventTypes, registeredType),
			"registry contains decoder for %q which is not in the allEventTypes list — either add it to allEventTypes if the event is legitimate, or remove the stale registry entry",
			registeredType)
	}
}
