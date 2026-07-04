package runtime

import (
	"strings"
	"sync"
)

// sessionEventHub broadcasts every event a session's runs produce to
// subscribers keyed by session id. It lets a TUI tab attach to an async
// subagent's sub-session and watch it live: the subagent manager stays the
// sole driver of the session, viewers only observe. Slow subscribers drop
// events rather than block a run.
//
// The hub also tracks each session's live run and in-flight (uncommitted)
// assistant message, accumulated from streaming deltas and reset at
// commit/turn boundaries. Subscribe snapshots both under the same lock that
// serializes Publish, so a viewer attaching mid-stream receives a synthetic
// StreamStarted plus the message's head as seed events and everything else
// through the channel — exactly once, with nothing needing repair after the
// fact.
type sessionEventHub struct {
	mu       sync.Mutex
	subs     map[string]map[chan Event]struct{}
	inflight map[string]*inflightAssistant
	// liveRuns counts StreamStarted minus StreamStopped per session, and
	// liveAgent remembers who is streaming, so a viewer attaching mid-run can
	// be seeded with the run's start it missed (tab spinners, working state).
	liveRuns  map[string]int
	liveAgent map[string]string
}

// inflightAssistant accumulates the streaming assistant message a session's
// live run has emitted so far. Mirrors the persistence observer's streaming
// state: reset on user-message, commit, error, and stream-stop events.
type inflightAssistant struct {
	agentName string
	content   strings.Builder
	reasoning strings.Builder
}

func newSessionEventHub() *sessionEventHub {
	return &sessionEventHub{
		subs:      map[string]map[chan Event]struct{}{},
		inflight:  map[string]*inflightAssistant{},
		liveRuns:  map[string]int{},
		liveAgent: map[string]string{},
	}
}

// Subscribe registers a watcher for a session's events. seed carries what the
// watcher missed, captured atomically with the registration: a synthetic
// StreamStarted when a run is live, then the in-flight assistant message's
// content so far (as synthetic delta events). Everything published before the
// snapshot is in seed, everything after arrives on the channel. The returned
// cancel is idempotent and closes the channel.
func (h *sessionEventHub) Subscribe(sessionID string, buffer int) (seed []Event, _ <-chan Event, cancel func()) {
	if buffer < 1 {
		buffer = 1
	}
	ch := make(chan Event, buffer)

	h.mu.Lock()
	if h.liveRuns[sessionID] > 0 {
		seed = append(seed, StreamStarted(sessionID, h.liveAgent[sessionID]))
	}
	if st := h.inflight[sessionID]; st != nil {
		if st.reasoning.Len() > 0 {
			seed = append(seed, AgentChoiceReasoning(st.agentName, sessionID, st.reasoning.String()))
		}
		if st.content.Len() > 0 {
			seed = append(seed, AgentChoice(st.agentName, sessionID, st.content.String()))
		}
	}
	if h.subs[sessionID] == nil {
		h.subs[sessionID] = map[chan Event]struct{}{}
	}
	h.subs[sessionID][ch] = struct{}{}
	h.mu.Unlock()

	var once sync.Once
	cancel = func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs[sessionID], ch)
			if len(h.subs[sessionID]) == 0 {
				delete(h.subs, sessionID)
			}
			h.mu.Unlock()
			close(ch)
		})
	}
	return seed, ch, cancel
}

// Publish delivers an event to every subscriber of the session (dropping it
// for subscribers whose buffer is full) and updates the session's in-flight
// assistant state.
func (h *sessionEventHub) Publish(sessionID string, event Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.trackInflightLocked(sessionID, event)
	for ch := range h.subs[sessionID] {
		select {
		case ch <- event:
		default:
		}
	}
}

// trackInflightLocked follows the session's stream lifecycle and accumulates
// streaming assistant deltas, resetting at the same boundaries the
// persistence observer uses (plus stream stop, so an aborted run cannot leak
// a stale prefix). Caller holds h.mu.
func (h *sessionEventHub) trackInflightLocked(sessionID string, event Event) {
	switch e := event.(type) {
	case *StreamStartedEvent:
		h.liveRuns[sessionID]++
		h.liveAgent[sessionID] = e.AgentName
		return
	case *AgentChoiceEvent:
		if e.Content == "" {
			return
		}
		st := h.inflightLocked(sessionID)
		st.agentName = e.AgentName
		st.content.WriteString(e.Content)
	case *AgentChoiceReasoningEvent:
		if e.Content == "" {
			return
		}
		st := h.inflightLocked(sessionID)
		st.agentName = e.AgentName
		st.reasoning.WriteString(e.Content)
	case *UserMessageEvent, *MessageAddedEvent, *ErrorEvent:
		delete(h.inflight, sessionID)
	case *StreamStoppedEvent:
		delete(h.inflight, sessionID)
		if h.liveRuns[sessionID] > 1 {
			h.liveRuns[sessionID]--
			return
		}
		delete(h.liveRuns, sessionID)
		delete(h.liveAgent, sessionID)
	}
}

func (h *sessionEventHub) inflightLocked(sessionID string) *inflightAssistant {
	st := h.inflight[sessionID]
	if st == nil {
		st = &inflightAssistant{}
		h.inflight[sessionID] = st
	}
	return st
}
