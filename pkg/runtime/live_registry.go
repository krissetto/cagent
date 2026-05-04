package runtime

import (
	"sync"
	"time"

	"github.com/docker/docker-agent/pkg/session"
)

// liveSessionRegistry tracks all sessions (root and subagent) that currently
// have a running engine. It is stored on [runtimeCore] so every
// [LocalRuntime] sharing the same core sees the same set.
//
// Both root and subagent sessions register here when their engine starts and
// unregister when it exits. [LiveSessionTree] reads the root node from this
// registry instead of synthesizing it from [LocalRuntime.CurrentAgentName].
type liveSessionRegistry struct {
	mu      sync.RWMutex
	entries map[string]liveSessionEntry
}

// liveSessionEntry is the per-session metadata stored in the registry.
type liveSessionEntry struct {
	sess      *session.Session
	agentName string
	kind      LiveSessionNodeKind
	createdAt time.Time
}

func newLiveSessionRegistry() *liveSessionRegistry {
	return &liveSessionRegistry{entries: make(map[string]liveSessionEntry)}
}

// register records a live session. Idempotent — a second call with the
// same session ID overwrites the previous entry.
func (r *liveSessionRegistry) register(sess *session.Session, agentName string, kind LiveSessionNodeKind) {
	r.mu.Lock()
	r.entries[sess.ID] = liveSessionEntry{
		sess:      sess,
		agentName: agentName,
		kind:      kind,
		createdAt: sess.CreatedAt,
	}
	r.mu.Unlock()
}

// unregister removes a session from the registry.
func (r *liveSessionRegistry) unregister(sessionID string) {
	r.mu.Lock()
	delete(r.entries, sessionID)
	r.mu.Unlock()
}

// get returns the entry for the given session id, or ok=false.
func (r *liveSessionRegistry) get(sessionID string) (liveSessionEntry, bool) {
	r.mu.RLock()
	e, ok := r.entries[sessionID]
	r.mu.RUnlock()
	return e, ok
}
