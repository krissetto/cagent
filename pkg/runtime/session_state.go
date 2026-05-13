package runtime

// sessionState bundles per-session coordination state that the run loop
// operates against. For the root session the fields are backed by the
// LocalRuntime's own queues and channels (a live view, not a copy);
// future PRs will allocate a fresh sessionState for child (subagent)
// sessions so each loop instance is independent.
type sessionState struct {
	// steerQueue holds urgent mid-turn messages. The loop drains ALL
	// pending messages after tool execution, before the stop check.
	steerQueue MessageQueue

	// followUpQueue holds end-of-turn messages. The loop pops exactly
	// ONE message after the model stops and stop-hooks have run.
	followUpQueue MessageQueue

	// resumeChan carries user decisions (approve / reject) when the
	// loop blocks on max-iterations or tool-approval prompts.
	resumeChan chan ResumeRequest

	// elicitationRequestCh receives elicitation responses from the
	// embedder (via ResumeElicitation).
	elicitationRequestCh chan ElicitationResult

	// elicitation owns the per-stream events channel used by the MCP
	// elicitation handler to send requests to the active stream.
	elicitation *elicitationBridge

	// wakePolicy controls the post-stop behaviour of the loop: root
	// sessions follow up / wait for subagent inbox; child sessions
	// publish envelopes and park. Nil means upstream default (no
	// subagent awareness).
	wakePolicy wakePolicy

	// isChild is true for subagent child sessions and indicates that
	// MCP elicitation requests must be auto-declined (children have no
	// user to answer prompts), and that per-turn ctx cancellation should
	// be treated as an interrupt+park rather than a fatal error.
	isChild bool

	// interruptedTurn is set when a child per-turn context is cancelled
	// but the outer session context is still alive. wakeNext consumes it
	// to park silently instead of publishing a terminal stop.
	interruptedTurn bool
}

// rootSessionState returns a sessionState backed by the runtime's own
// fields. Every field is a reference type (interface, channel, or
// pointer) so the returned struct is a live view — mutations through
// the sessionState are visible on the runtime and vice-versa.
func (r *LocalRuntime) rootSessionState() *sessionState {
	return &sessionState{
		steerQueue:           r.steerQueue,
		followUpQueue:        r.followUpQueue,
		resumeChan:           r.resumeChan,
		elicitationRequestCh: r.elicitationRequestCh,
		elicitation:          &r.elicitation,
		wakePolicy:           r.rootWakePolicyFor(),
	}
}

// rootWakePolicyFor returns a rootWakePolicy for root sessions when the
// subagent manager is active, or nil when no subagents are configured
// (preserving upstream behaviour exactly).
func (r *LocalRuntime) rootWakePolicyFor() wakePolicy {
	if r.subagents == nil {
		return nil
	}
	return rootWakePolicy{}
}
