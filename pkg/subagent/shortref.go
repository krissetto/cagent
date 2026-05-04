package subagent

// ShortRefLen is the number of leading characters of a subagent's session id
// exposed to the LLM. It is intentionally short so language models can reliably
// reproduce the id across tool calls without mangling it, while still giving
// enough entropy within the scope of a single parent to disambiguate.
//
// Internally the runtime continues to use the full session id for storage,
// events, and cross-process APIs; ShortRef is only used at the
// subagent-tool boundary (tool responses, envelope reminders) and at the
// resolver that maps a short ref back to a full id for the caller's direct
// children. See [Manager.ResolveChildRef].
const ShortRefLen = 5

// ShortRef returns the short, model-friendly reference for a subagent id.
// Inputs shorter than [ShortRefLen] are returned as-is so tests and fixtures
// that use synthetic ids continue to work.
func ShortRef(id string) string {
	if len(id) <= ShortRefLen {
		return id
	}
	return id[:ShortRefLen]
}
