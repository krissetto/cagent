package runtime

import "sync"

// sessionState bundles the per-live-session mutable coordination state that
// today is scattered across [LocalRuntime] fields. It is the structural
// counterpart to [runtimeCore]:
//
//   - [runtimeCore] holds runtime-wide shared services (team, stores, manager,
//     event bus, ...). It is safe to share across every live session.
//   - [sessionState] holds the per-session channels and flags that drive a
//     single live session's outer loop. Each live session must have its own.
//
// Today, [LocalRuntime] embeds *sessionState as an anonymous pointer so every
// field below is reachable through promoted-field access (e.g.
// `r.resumeChan`, `r.steer`). A child [sessionRunner] (see
// [newChildSessionRunner]) just allocates a fresh *sessionState and
// reuses the same *runtimeCore pointer.
//
// Field ownership notes:
//
//   - steer: urgent mid-turn messages injected by the user mid-turn; the
//     loop drains ALL pending messages after tool execution.
//   - followUp: end-of-turn messages; the loop pops exactly ONE after the
//     model stops and re-enters for a fresh turn.
//   - resumeChan: delivers ResumeRequest from external callers (TUI, server)
//     to the loop waiting on tool confirmation / max-iterations approval.
//   - elicitationRequestCh: delivers ElicitationResult from external callers
//     to a loop currently servicing an elicitation request.
//   - elicitationEventsChannel + Mux: the events channel currently used to
//     forward elicitation requests outward. Swapped in/out per RunStream
//     invocation so nested streams (sub-sessions, background agents) don't
//     lose the parent's channel.
//   - currentAgent + Mu: this session's pinned agent identity. For root
//     sessions, this is the runtime-wide selection (mutated by
//     SetCurrentAgent); for child sessions it is fixed at construction.
//   - startupInfoEmitted: gate for one-shot startup metadata emission per
//     session.
//
// Field names intentionally match the historical [LocalRuntime] field names
// so promoted-field access (`r.resumeChan`, `r.steer`, `r.currentAgent`...)
// keeps working without churning every call site.
type sessionState struct {
	steer    MessageQueue
	followUp MessageQueue

	resumeChan           chan ResumeRequest
	elicitationRequestCh chan ElicitationResult

	elicitationEventsChannelMux sync.RWMutex
	elicitationEventsChannel    chan Event

	currentAgentMu sync.RWMutex
	currentAgent   string

	startupInfoEmitted bool
}

// currentAgentName returns this session's pinned agent name in a
// concurrency-safe way. Each sessionState owns its own currentAgentMu;
// callers reach this helper through their *sessionRunner so root and
// child sessions never share the same lock.
func (s *sessionState) currentAgentName() string {
	s.currentAgentMu.RLock()
	defer s.currentAgentMu.RUnlock()
	return s.currentAgent
}

// newSessionState allocates a fresh per-session state bag with sensible
// zero values for every coordination channel. currentAgent pins this
// session's agent identity (use the runtime's default-agent name for root
// sessions and the child agent name for subagent sessions).
func newSessionState(currentAgent string) *sessionState {
	return &sessionState{
		steer:                NewInMemoryMessageQueue(defaultSteerQueueCapacity),
		followUp:             NewInMemoryMessageQueue(defaultFollowUpQueueCapacity),
		resumeChan:           make(chan ResumeRequest),
		elicitationRequestCh: make(chan ElicitationResult),
		currentAgent:         currentAgent,
	}
}
