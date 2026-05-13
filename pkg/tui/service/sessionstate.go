package service

import (
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
	"github.com/docker/docker-agent/pkg/userconfig"
)

// SessionStateReader provides read-only access to session state.
// Components that only need to read state should depend on this interface
// rather than the full SessionState, following the principle of least privilege.
type SessionStateReader interface {
	SplitDiffView() bool
	YoloMode() bool
	HideToolResults() bool
	CurrentAgentName() string
	PreviousMessage() *types.Message
	SessionTitle() string
	AvailableAgents() []runtime.AgentDetails
	GetCurrentAgent() runtime.AgentDetails

	// IsSubSession reports whether the state belongs to a descendant (attached
	// child-session) tab. The UI uses this to suppress per-message agent
	// badges that are redundant in a single-agent view.
	IsSubSession() bool

	// RootSessionID returns the root session id for the current live session
	// tree. For owned tabs this is the tab's own session id; for attached
	// descendant tabs it is the root ancestor session id. The UI uses this when
	// it needs a whole-tree view (e.g. resolving nested subagents or rendering
	// a full session-tree dialog) instead of just the immediate parent chain.
	RootSessionID() string

	// ParentSessionID returns the parent session id for attached
	// sub-session tabs and "" for owned (root) tabs. It is populated by the
	// tab-opening code from the live-session node so the sidebar can
	// surface a clickable "parent: …" line that jumps back to the parent.
	ParentSessionID() string

	// ParentAgentName returns the parent agent's name for attached
	// sub-session tabs and "" for owned (root) tabs. It is populated at
	// tab-open time so the sidebar / transcript banner can render a
	// "[parent] ↔ [child]" badge before any runtime event arrives.
	ParentAgentName() string
}

// Verify SessionState implements SessionStateReader
var _ SessionStateReader = (*SessionState)(nil)

// SessionState holds shared state across the TUI application.
// This provides a centralized location for state that needs to be
// accessible by multiple components.
type SessionState struct {
	splitDiffView   bool
	yoloMode        bool
	hideToolResults bool
	sessionTitle    string
	isSubSession    bool
	rootSessionID   string
	parentSessionID string
	parentAgentName string

	previousMessage  *types.Message
	currentAgentName string
	availableAgents  []runtime.AgentDetails
}

func NewSessionState(s *session.Session) *SessionState {
	return &SessionState{
		splitDiffView:   userconfig.Get().GetSplitDiffView(),
		yoloMode:        s.ToolsApproved,
		hideToolResults: s.HideToolResults,
		// Session titles may be updated by background runtime goroutines (e.g.
		// attached subagent title generation), so read them through GetTitle().
		sessionTitle:    s.GetTitle(),
		isSubSession:    s.ParentID != "",
		rootSessionID:   s.ID,
		parentSessionID: s.ParentID,
	}
}

func (s *SessionState) SplitDiffView() bool {
	return s.splitDiffView
}

func (s *SessionState) ToggleSplitDiffView() {
	s.splitDiffView = !s.splitDiffView
}

func (s *SessionState) YoloMode() bool {
	return s.yoloMode
}

func (s *SessionState) SetYoloMode(yoloMode bool) {
	s.yoloMode = yoloMode
}

func (s *SessionState) HideToolResults() bool {
	return s.hideToolResults
}

func (s *SessionState) ToggleHideToolResults() {
	s.hideToolResults = !s.hideToolResults
}

func (s *SessionState) SetHideToolResults(hideToolResults bool) {
	s.hideToolResults = hideToolResults
}

func (s *SessionState) CurrentAgentName() string {
	return s.currentAgentName
}

func (s *SessionState) SetCurrentAgentName(currentAgentName string) {
	s.currentAgentName = currentAgentName
}

func (s *SessionState) PreviousMessage() *types.Message {
	return s.previousMessage
}

func (s *SessionState) SetPreviousMessage(previousMessage *types.Message) {
	s.previousMessage = previousMessage
}

func (s *SessionState) SessionTitle() string {
	return s.sessionTitle
}

func (s *SessionState) SetSessionTitle(sessionTitle string) {
	s.sessionTitle = sessionTitle
}

func (s *SessionState) AvailableAgents() []runtime.AgentDetails {
	return s.availableAgents
}

func (s *SessionState) SetAvailableAgents(availableAgents []runtime.AgentDetails) {
	s.availableAgents = availableAgents

	names := make([]string, len(availableAgents))
	for i, a := range availableAgents {
		names[i] = a.Name
	}
	styles.SetAgentOrder(names)
}

func (s *SessionState) GetCurrentAgent() runtime.AgentDetails {
	for _, agent := range s.availableAgents {
		if agent.Name == s.currentAgentName {
			return agent
		}
	}

	return runtime.AgentDetails{}
}

// IsSubSession reports whether this state represents an attached child-session
// tab (a live descendant of another session's agent tree).
func (s *SessionState) IsSubSession() bool {
	if s == nil {
		return false
	}
	return s.isSubSession
}

// SetSubSession lets callers explicitly mark/unmark the state as a
// sub-session view. Normally this is derived from the session's ParentID at
// construction time, but some callers (supervisor-registered attached tabs)
// build the state before the session pointer settles.
func (s *SessionState) SetSubSession(sub bool) {
	s.isSubSession = sub
}

// RootSessionID returns the root ancestor session id for the current
// sub-session tree. Owned tabs return their own session id.
func (s *SessionState) RootSessionID() string {
	if s == nil {
		return ""
	}
	return s.rootSessionID
}

// SetRootSessionID explicitly records the root ancestor id for the current
// session tree.
func (s *SessionState) SetRootSessionID(id string) {
	if s == nil || id == "" {
		return
	}
	s.rootSessionID = id
}

// ParentSessionID returns the parent session id for attached sub-session
// tabs. Owned tabs return "".
func (s *SessionState) ParentSessionID() string {
	if s == nil {
		return ""
	}
	return s.parentSessionID
}

// SetParentSessionID explicitly sets the parent session id. Used by the
// tab-opening path when a fresh session state is constructed before the
// session metadata is fully resolved.
func (s *SessionState) SetParentSessionID(id string) {
	if s == nil {
		return
	}
	s.parentSessionID = id
	if id != "" {
		s.isSubSession = true
	}
}

// ParentAgentName returns the parent agent's name for attached sub-session
// tabs. Owned tabs return "".
func (s *SessionState) ParentAgentName() string {
	if s == nil {
		return ""
	}
	return s.parentAgentName
}

// SetParentAgentName records the parent agent's display name so the sidebar
// and transcript banner can surface the "[parent] ↔ [child]" relationship
// without reaching back into the runtime.
func (s *SessionState) SetParentAgentName(name string) {
	if s == nil {
		return
	}
	s.parentAgentName = name
}
