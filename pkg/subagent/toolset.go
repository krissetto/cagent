package subagent

import (
	"context"

	"github.com/docker/docker-agent/pkg/tools"
)

const (
	ToolNameStart    = "subagent_start"
	ToolNameSend     = "subagent_send"
	ToolNameList     = "subagent_list"
	ToolNameInspect  = "subagent_inspect"
	ToolNameFinalize = "subagent_finalize"
	// ToolNameClose is the deprecated alias for [ToolNameFinalize]. The
	// runtime still dispatches it so old session recordings replay
	// correctly, but the toolset deliberately does not advertise it to
	// the model anymore: exposing two names with the same semantics led
	// the model to grab whichever verb felt more natural rather than the
	// canonical one. New callers must use [ToolNameFinalize].
	ToolNameClose = "subagent_close"
	ToolNameStop  = "subagent_stop"
)

// Inspect modes control how much context subagent_inspect returns to the
// caller. Smaller modes mean less context cost and less pollution of the
// parent agent's window.
const (
	// InspectModeLast returns only the subagent's last assistant message.
	// Cheapest option; preferred when the parent just wants the latest reply.
	InspectModeLast = "last"
	// InspectModeRecent returns the last assistant message plus up to
	// [InspectRecentLimit] most-recent non-system messages.
	InspectModeRecent = "recent"
	// InspectModeFull returns the last assistant message plus the entire
	// non-system transcript. Use sparingly — this can dump a lot of tokens
	// into the parent's context.
	InspectModeFull = "full"

	// InspectRecentLimit is the number of non-system messages included in
	// InspectModeRecent responses.
	InspectRecentLimit = 6
)

// DefaultInspectMode is returned when the caller does not supply a `mode`.
// It intentionally biases toward the cost-aware path; callers can opt in to
// richer payloads with [InspectModeRecent] or [InspectModeFull].
const DefaultInspectMode = InspectModeLast

const (
	subagentIDDescription = "The short ID of the sub-agent session (the first 5 characters returned by subagent_start or shown in subagent_list / subagent updates). Full IDs are also accepted."
	// InspectFullMaxBytes caps the JSON-encoded byte size of the transcript
	// payload returned by subagent_inspect mode="full". We bound it so a
	// model cannot accidentally stuff an arbitrarily large child transcript
	// back into the parent context in one tool response.
	InspectFullMaxBytes = 64 * 1024
)

// NormalizeInspectMode returns a canonical mode string, falling back to
// [DefaultInspectMode] for empty or unrecognised inputs.
func NormalizeInspectMode(mode string) string {
	switch mode {
	case InspectModeLast, InspectModeRecent, InspectModeFull:
		return mode
	default:
		return DefaultInspectMode
	}
}

// StartArgs starts a new runtime-managed subagent conversation.
type StartArgs struct {
	Agent string `json:"agent" jsonschema:"The name of the sub-agent to start."`
	Task  string `json:"task" jsonschema:"A concise description of the task the sub-agent should begin with."`
}

// SendArgs sends a message to a live subagent.
type SendArgs struct {
	SubAgentID string `json:"subagent_id" jsonschema:"The short ID of the sub-agent session (the first 5 characters returned by subagent_start or shown in subagent_list / subagent updates). Full IDs are also accepted."`
	Message    string `json:"message" jsonschema:"The message to send to the sub-agent."`
	Mode       string `json:"mode,omitempty" jsonschema:"How to deliver the message. One of: 'followup' (default, empty) — plain user turn injected between the sub-agent's turns; 'steer' — injected at the next safe point if the sub-agent is currently running (mid-turn), otherwise between turns. Use 'steer' for urgent course corrections you want the sub-agent to see immediately."`
}

// InspectArgs returns a view of a child subagent.
//
// The amount of context returned is controlled by Mode. The default
// ([DefaultInspectMode]) is the cheapest option: only the subagent's latest
// assistant message is returned, along with status metadata. Use
// [InspectModeRecent] to also receive up to [InspectRecentLimit] recent
// non-system messages, or [InspectModeFull] to receive the entire
// non-system transcript.
type InspectArgs struct {
	SubAgentID string `json:"subagent_id" jsonschema:"The short ID of the sub-agent session (the first 5 characters returned by subagent_start or shown in subagent_list / subagent updates). Full IDs are also accepted."`
	Mode       string `json:"mode,omitempty" jsonschema:"How much context to return. One of: 'last' (default) — only the subagent's latest assistant message plus status metadata (cheapest, keeps parent context small); 'recent' — also include up to 6 most-recent non-system messages; 'full' — include the entire non-system transcript, truncated to roughly 64KB if very long (expensive, use only when 'recent' isn't enough)."`
}

// FinalizeArgs requests a graceful finalize.
type FinalizeArgs struct {
	SubAgentID string `json:"subagent_id" jsonschema:"The short ID of the sub-agent session (the first 5 characters returned by subagent_start or shown in subagent_list / subagent updates). Full IDs are also accepted."`
}

// CloseArgs requests a graceful close.
//
// Deprecated: use [FinalizeArgs]. Kept for wire compatibility with older
// sessions / recordings that still refer to `subagent_close`.
type CloseArgs = FinalizeArgs

// StopArgs requests an immediate stop.
type StopArgs struct {
	SubAgentID string `json:"subagent_id" jsonschema:"The short ID of the sub-agent session (the first 5 characters returned by subagent_start or shown in subagent_list / subagent updates). Full IDs are also accepted."`
}

// NewToolSet returns a lightweight toolset exposing only tool metadata and
// instructions. The runtime wires the handlers directly.
func NewToolSet() tools.ToolSet { return &toolSet{} }

type toolSet struct{}

func (t *toolSet) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{
		{
			Name:     ToolNameStart,
			Category: "transfer",
			Description: `Start a persistent runtime-managed sub-agent in the background.
The sub-agent gets its own session and can continue sending updates back to you
without polling. Use subagent_send to continue the conversation later.`,
			Parameters: tools.MustSchemaFor[StartArgs](),
			Annotations: tools.ToolAnnotations{
				Title: "Start Subagent",
			},
		},
		{
			Name:     ToolNameSend,
			Category: "transfer",
			Description: `Send a message to a live sub-agent so it can continue
its work or answer a question.

By default the message is delivered as a regular follow-up between the
sub-agent's turns. Pass mode="steer" to instead deliver it mid-turn (at
the next safe point) as an urgent course correction that the sub-agent
will see immediately without waiting for its current turn to finish.`,
			Parameters: tools.MustSchemaFor[SendArgs](),
			Annotations: tools.ToolAnnotations{
				Title: "Message Subagent",
			},
		},
		{
			Name:        ToolNameList,
			Category:    "transfer",
			Description: `List runtime-managed sub-agents for the current parent session.`,
			Annotations: tools.ToolAnnotations{Title: "List Subagents", ReadOnlyHint: true},
		},
		{
			Name:     ToolNameInspect,
			Category: "transfer",
			Description: `Inspect a runtime-managed sub-agent. Returns its current status and its
last assistant message in full. By default the response does NOT include the
prior transcript, to keep your context window small and your token cost low.
Pass mode="recent" (last few messages) or mode="full" (entire non-system
transcript) only when you actually need that extra context — for example when
the last reply references earlier turns you haven't already seen.`,
			Parameters:  tools.MustSchemaFor[InspectArgs](),
			Annotations: tools.ToolAnnotations{Title: "Inspect Subagent", ReadOnlyHint: true},
		},
		{
			Name:        ToolNameFinalize,
			Category:    "transfer",
			Description: `Ask a runtime-managed sub-agent to finalize cleanly after its current safe point.`,
			Parameters:  tools.MustSchemaFor[FinalizeArgs](),
			Annotations: tools.ToolAnnotations{Title: "Finalize Subagent"},
		},
		{
			Name:        ToolNameStop,
			Category:    "transfer",
			Description: `Immediately stop a runtime-managed sub-agent.`,
			Parameters:  tools.MustSchemaFor[StopArgs](),
			Annotations: tools.ToolAnnotations{Title: "Stop Subagent"},
		},
	}, nil
}

func (t *toolSet) Instructions() string {
	return `# Runtime-managed subagents

Use runtime-managed subagents when you want a persistent background conversation
with another agent.

- **subagent_start**: create a new persistent subagent session
- **subagent_send**: send a message to a running/waiting subagent.
  Defaults to follow-up mode (plain user turn between turns). Pass
  mode="steer" to interrupt a running sub-agent with an urgent mid-turn
  course correction at the next safe point.
- **subagent_list**: list subagents owned by the current session
- **subagent_inspect**: inspect a specific subagent session. By default
  returns only the latest assistant message (cheap, context-friendly).
  Pass mode="recent" for the last few messages or mode="full" for the
  whole transcript — only when you actually need prior context.
- **subagent_finalize**: ask a subagent to finalize cleanly
- **subagent_stop**: cancel a subagent immediately

When a subagent finishes a turn, the runtime will send you a compact update
message automatically. You do not need to poll for completion.

Important: when a tool returns a subagent_id, use the short id exactly as shown
(the first 5 characters) in later subagent tool calls. Full IDs are accepted,
but the short id is preferred because models reproduce it more reliably.`
}
