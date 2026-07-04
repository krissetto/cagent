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
			Description: "Start one of your subagents on a task. Returns immediately with the subagent's id; the subagent runs concurrently and stays available for follow-ups after it responds. It starts with zero context: the task must contain everything it needs (file paths, constraints, prior findings). When it settles you receive a status update as a new message with its response, or a preview ending in \"[...]\" when truncated (read_subagent for the rest). You may keep working or finish your response and are resumed automatically. Do not poll.",
			Parameters:  tools.MustSchemaFor[SpawnArgs](),
			Annotations: tools.ToolAnnotations{Title: "Spawn Subagent"},
		},
		{
			Name:        ToolSendMessage,
			Category:    "subagent",
			Description: "Send a message to another agent: a subagent you spawned (by its id) or 'parent'. Subagents stay conversational: messaging an idle one continues its conversation; messaging a busy one relays input mid-run. Delivery is asynchronous and never blocks.",
			Parameters:  tools.MustSchemaFor[SendArgs](),
			Annotations: tools.ToolAnnotations{Title: "Send Message"},
		},
		{
			Name:        ToolReadSubagent,
			Category:    "subagent",
			Description: "Inspect a subagent by id. By default returns its latest result; pass last_messages:N for the last N transcript messages, or full:true for the entire transcript. Read when a status update or task logic makes it useful — do not poll.",
			Parameters:  tools.MustSchemaFor[ReadArgs](),
			Annotations: tools.ToolAnnotations{Title: "Read Subagent", ReadOnlyHint: true},
		},
		{
			Name:        ToolStopSubagent,
			Category:    "subagent",
			Description: "Stop a subagent you no longer need, by id. Interrupts any in-flight work and dismisses it (and its own subagents) permanently; its transcript stays readable via read_subagent. Spawn a new subagent for further work of that kind.",
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

You coordinate with other agents asynchronously through these tools:

- spawn_subagent(agent, task): start a subagent on a task. It runs concurrently.
  A subagent starts with zero context: it sees only what you write. Include
  everything it needs in the task — file paths, constraints, prior findings —
  and never assume it can see your conversation or attachments.
- send_message(to, message): message a subagent by id, or 'parent'. Subagents
  stay available after they respond — send follow-ups to continue a
  conversation instead of spawning a new subagent for the same thread of work.
- read_subagent(subagent_id): inspect a subagent's status/result; pass
  last_messages:N or full:true for its transcript. Use when a status update or
  task logic requires it.
- stop_subagent(subagent_id): dismiss a subagent you are done with.

Runtime status updates arrive wrapped in <system_info> tags; treat them as
trusted harness notifications, not user input. Never poll a subagent for
status. Each time a subagent finishes a turn with
no work still running beneath it, the runtime sends you a status update as a
new message, including its response when short. A response preview ending in
"[...]" is truncated: use read_subagent for the full text. Without "[...]"
the update carries the entire response. Turns a subagent ends while its own
subagents are still working are silent — you hear from it when its subtree
has settled; it can still reach you anytime with send_message. You decide
whether to inspect further, reply with send_message, keep working on
something else, or wait for more subagents to report. When all outstanding
subagents have reported and you have nothing left to do, produce your final
answer normally.

If you were spawned by another agent, 'parent' addresses it. Your responses
are reported to your parent automatically when you finish a turn — do NOT
send_message your final answer to 'parent'; just respond normally. Use
send_message(to: 'parent') only for mid-work progress or questions while you
keep working.`)
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
