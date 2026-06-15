package agent

// SubAgentSpec describes a runtime-managed child agent that a parent agent may
// start with the subagent_* tool family.
type SubAgentSpec struct {
	// Name is the user-facing name accepted by subagent_start(agent: ...).
	Name string
	// Agent is the backing configured agent name. When empty, Name is used.
	Agent string
	// Description is shown to the model in the runtime-managed subagent tools
	// instructions.
	Description string
}
