package runtime

import "sync"

type liveSessionRegistry struct {
	mu       sync.RWMutex
	sessions map[string]liveSessionInfo
}

type liveSessionInfo struct {
	SessionID string
	AgentName string
	ParentID  string
}

func newLiveSessionRegistry() *liveSessionRegistry {
	return &liveSessionRegistry{sessions: make(map[string]liveSessionInfo)}
}

func (r *liveSessionRegistry) register(sessionID, agentName, parentID string) {
	if r == nil || sessionID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessions[sessionID] = liveSessionInfo{SessionID: sessionID, AgentName: agentName, ParentID: parentID}
}

func (r *liveSessionRegistry) unregister(sessionID string) {
	if r == nil || sessionID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sessions, sessionID)
}

func (r *liveSessionRegistry) get(sessionID string) (liveSessionInfo, bool) {
	if r == nil || sessionID == "" {
		return liveSessionInfo{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	info, ok := r.sessions[sessionID]
	return info, ok
}
