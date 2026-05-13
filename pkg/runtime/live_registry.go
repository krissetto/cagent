package runtime

import "sync"

// liveSessionRegistry tracks all sessions (root and subagent) that currently
// have a running loop within a LocalRuntime.
type liveSessionRegistry struct {
	mu      sync.RWMutex
	entries map[string]liveSessionEntry
}

type liveSessionEntry struct {
	id        string
	agentName string
	parentID  string
}

func newLiveSessionRegistry() *liveSessionRegistry {
	return &liveSessionRegistry{entries: make(map[string]liveSessionEntry)}
}

// register records a live session. Idempotent — a second call with the
// same session ID overwrites the previous entry.
func (r *liveSessionRegistry) register(id, agentName, parentID string) {
	if r == nil || id == "" {
		return
	}
	r.mu.Lock()
	r.entries[id] = liveSessionEntry{id: id, agentName: agentName, parentID: parentID}
	r.mu.Unlock()
}

// unregister removes a session from the registry.
func (r *liveSessionRegistry) unregister(sessionID string) {
	if r == nil || sessionID == "" {
		return
	}
	r.mu.Lock()
	delete(r.entries, sessionID)
	r.mu.Unlock()
}

// get returns the entry for the given session id, or ok=false.
func (r *liveSessionRegistry) get(sessionID string) (liveSessionEntry, bool) {
	if r == nil || sessionID == "" {
		return liveSessionEntry{}, false
	}
	r.mu.RLock()
	e, ok := r.entries[sessionID]
	r.mu.RUnlock()
	return e, ok
}

// all returns a snapshot of all live registry entries.
func (r *liveSessionRegistry) all() []liveSessionEntry {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	out := make([]liveSessionEntry, 0, len(r.entries))
	for _, e := range r.entries {
		out = append(out, e)
	}
	r.mu.RUnlock()
	return out
}
