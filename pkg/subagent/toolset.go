package subagent

import (
	"context"
	"strings"

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

// ToolSetAgentSpec describes one subagent available to the parent agent.
// It is used to populate the JSON schema enum and tool description so the
// model knows which agent names are valid for subagent_start.
type ToolSetAgentSpec struct {
	Name        string
	Description string
}

// NewToolSet returns a lightweight toolset exposing only tool metadata and
// instructions. The runtime wires the handlers directly.
//
// specs lists the subagent names (and optional descriptions) available to
// the parent agent. The subagent_start tool's `agent` parameter is
// constrained to an enum of those names, and the tool description and
// instructions list them explicitly so the model cannot hallucinate agent
// names that don't exist in the team.
func NewToolSet(specs []ToolSetAgentSpec) tools.ToolSet { return &toolSet{specs: specs} }

type toolSet struct {
	specs []ToolSetAgentSpec
}

func (t *toolSet) Tools(context.Context) ([]tools.Tool, error) {
	names := make([]string, len(t.specs))
	var listing strings.Builder
	for i, s := range t.specs {
		names[i] = s.Name
		listing.WriteString("\n- " + s.Name)
		if s.Description != "" {
			listing.WriteString(": " + s.Description)
		}
	}

	startDesc := `Start a persistent runtime-managed sub-agent in the background.
The sub-agent gets its own session and reports back through automatic runtime
updates. Send follow-up work with subagent_send when you need it to continue.

Available sub-agents:` + listing.String()

	return []tools.Tool{
		{
			Name:        ToolNameStart,
			Category:    "transfer",
			Description: startDesc,
			Parameters:  buildStartSchemaWithEnum(names),
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
			Description: `Read more context from a runtime-managed sub-agent session. Automatic
runtime updates already include status and a preview, so inspect is mainly for
cases where the preview omits details you need. The default mode returns only
the latest assistant message; use mode="recent" for a short transcript slice or
mode="full" only when the broader transcript is required.`,
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

// buildStartSchemaWithEnum returns a JSON-schema map for StartArgs where the
// "agent" property is constrained to the supplied enum values.
func buildStartSchemaWithEnum(names []string) map[string]any {
	enumVals := make([]any, len(names))
	for i, n := range names {
		enumVals[i] = n
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"agent": map[string]any{
				"type":        "string",
				"description": "The name of the sub-agent to start.",
				"enum":        enumVals,
			},
			"task": map[string]any{
				"type":        "string",
				"description": "A concise description of the task the sub-agent should begin with.",
			},
		},
		"required": []string{"agent", "task"},
	}
}

func (t *toolSet) Instructions() string {
	var b strings.Builder
	b.WriteString("\n\nAvailable sub-agents that can be started:")
	for _, s := range t.specs {
		b.WriteString("\n- " + s.Name)
		if s.Description != "" {
			b.WriteString(": " + s.Description)
		}
	}
	b.WriteString("\n\nOnly these agent names may be passed to subagent_start.")
	agentList := b.String()

	return `# Runtime-managed subagents

Use runtime-managed subagents for persistent background work with another
agent. Start a subagent with a self-contained task, then let runtime updates
bring its progress back into this conversation.

- **subagent_start**: start a persistent subagent session.
- **subagent_send**: give an existing subagent more work or answer a question.
  Use the default follow-up mode for normal messages; use mode="steer" for an
  urgent course correction while the subagent is mid-turn.
- **subagent_inspect**: read transcript content when an automatic update's
  preview is not enough. Default mode returns the latest assistant message;
  use mode="recent" or mode="full" only when that extra context is needed.
- **subagent_list**: list current subagents when you have lost track of ids.
- **subagent_finalize**: ask a subagent to finish cleanly.
- **subagent_stop**: cancel a subagent immediately.

## Operating model

- Runtime updates are push-based. After starting or messaging a subagent, wait
  for the next automatic update instead of checking for status.
- Keep delegated ownership clear. Once a subagent owns a task, let it finish
  that task; use your own time for different work or synthesis, not duplicate
  implementation of the delegated work.
- Treat inspect as transcript retrieval. Use it when you need details omitted
  from an update preview, then continue from that information.
- Use the short subagent_id shown by subagent_start or runtime updates in later
  subagent tool calls.` + agentList
}
