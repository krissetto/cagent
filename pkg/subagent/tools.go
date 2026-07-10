package subagent

import (
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/tools"
)

// Core tool names. The set is intentionally tiny so small models can drive the
// harness reliably. There is no end_turn tool: an agent ends its turn by simply
// finishing its response, exactly as it would normally. Subagents are
// persistent conversational actors: they stay available after each response
// (send_message continues the conversation) until their parent explicitly
// stops them with stop_subagent.
const (
	ToolSpawnSubagent = "spawn_subagent"
	ToolSendMessage   = "send_message"
	ToolReadSubagent  = "read_subagent"
	ToolStopSubagent  = "stop_subagent"
)

// AllowedSubagent is a subagent a node may spawn, as seen by the model.
type AllowedSubagent struct {
	Agent       string
	Name        string
	Description string
}

// DisplayName returns the model-facing name (alias or agent name).
func (a AllowedSubagent) DisplayName() string {
	if a.Name != "" {
		return a.Name
	}
	return a.Agent
}

// SpawnArgs are the arguments for spawn_subagent.
type SpawnArgs struct {
	Agent string `json:"agent" jsonschema:"The name of the subagent to start (must be one of your declared subagents)."`
	Task  string `json:"task" jsonschema:"A clear, self-contained description of what the subagent should do."`
}

// SendArgs are the arguments for send_message.
type SendArgs struct {
	To      string `json:"to" jsonschema:"The id of the agent to message (a subagent id returned by spawn_subagent, or 'parent')."`
	Message string `json:"message" jsonschema:"The message body to deliver."`
}

// ReadArgs are the arguments for read_subagent.
type ReadArgs struct {
	SubagentID   string `json:"subagent_id" jsonschema:"The id returned by spawn_subagent."`
	LastMessages int    `json:"last_messages,omitempty" jsonschema:"Return the last N messages of the subagent's transcript instead of just its final result."`
	Full         bool   `json:"full,omitempty" jsonschema:"Return the subagent's full transcript instead of just its final result."`
}

// StopArgs are the arguments for stop_subagent.
type StopArgs struct {
	SubagentID string `json:"subagent_id" jsonschema:"The id of the subagent to stop."`
}

// ParentAlias is the reserved target that addresses a node's parent.
const ParentAlias = "parent"

// Definitions returns the core tool definitions. The runtime injects these onto
// any agent that declares subagents; there is no toolset to configure.
func Definitions() []tools.Tool {
	return []tools.Tool{
		{
			Name:        ToolSpawnSubagent,
			Category:    "subagent",
			Description: "Start a subagent on a task. Returns its id immediately; it runs concurrently and keeps its session for follow-ups. The task is the subagent's entire starting context, so include all needed details. When its subtree is quiet, you receive a system_info status with its result or a truncated preview. You may continue working or end your turn to wait; do not poll.",
			Parameters:  tools.MustSchemaFor[SpawnArgs](),
			Annotations: tools.ToolAnnotations{Title: "Spawn Subagent"},
		},
		{
			Name:        ToolSendMessage,
			Category:    "subagent",
			Description: "Send a message to a subagent by id, or to 'parent'. Idle subagents start a new turn; busy subagents receive the message mid-run. Delivery is asynchronous and never blocks.",
			Parameters:  tools.MustSchemaFor[SendArgs](),
			Annotations: tools.ToolAnnotations{Title: "Send Message"},
		},
		{
			Name:        ToolReadSubagent,
			Category:    "subagent",
			Description: "Inspect a subagent by id. By default returns its latest result; pass last_messages:N for recent transcript lines, or full:true for the whole transcript. Read when a status update or task logic requires it; do not poll.",
			Parameters:  tools.MustSchemaFor[ReadArgs](),
			Annotations: tools.ToolAnnotations{Title: "Read Subagent", ReadOnlyHint: true},
		},
		{
			Name:        ToolStopSubagent,
			Category:    "subagent",
			Description: "Stop a subagent you no longer need. This cancels its current work, stops its descendants, and prevents future messages; its transcript remains readable.",
			Parameters:  tools.MustSchemaFor[StopArgs](),
			Annotations: tools.ToolAnnotations{Title: "Stop Subagent"},
		},
	}
}

// Instructions returns harness guidance appended to the system prompt for the
// core tools.
func Instructions() string {
	return strings.TrimSpace(`
# Subagent harness

Use these tools for async delegation:

- spawn_subagent(agent, task): start a subagent. It runs concurrently and keeps
  its session for follow-ups. The task is its entire starting context; include
  all details it needs.
- send_message(to, message): message a subagent by id, or 'parent'. Idle
  subagents start a new turn; busy subagents receive the message mid-run.
- read_subagent(subagent_id): inspect status/result or transcript when needed.
  Do not poll.
- stop_subagent(subagent_id): stop a subagent and its descendants permanently.

Status updates arrive as <system_info> harness messages. A subagent reports only
when its own subtree is quiet; if it ends a turn while its children still work,
you hear nothing until that subtree settles. Previews ending in "[...]" are
truncated; use read_subagent for the full text.

To wait, first make sure work is running below you (spawn/message a subagent),
then finish your response. If nothing below you is running, nothing will wake
you. When all needed reports arrive, answer normally.

If you were spawned, 'parent' addresses your parent. Your final response is
reported automatically when you finish; use send_message(to: 'parent') only for
progress or questions while you keep working.`)
}

// FindAllowed resolves a model-supplied subagent name against an agent's
// allow-list, matching the display name (alias) or the underlying agent name.
func FindAllowed(allowed []AllowedSubagent, name string) (AllowedSubagent, bool) {
	idx := slices.IndexFunc(allowed, func(a AllowedSubagent) bool {
		return a.DisplayName() == name || a.Agent == name
	})
	if idx < 0 {
		return AllowedSubagent{}, false
	}
	return allowed[idx], true
}
