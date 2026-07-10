package supervisor

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
)

func newTestSupervisor(ids []string, activeID string) *Supervisor {
	s := &Supervisor{
		runners:      make(map[string]*SessionRunner),
		programReady: make(chan struct{}),
	}
	for _, id := range ids {
		s.runners[id] = &SessionRunner{ID: id}
		s.order = append(s.order, id)
	}
	s.activeID = activeID
	return s
}

func TestCloseSession_FocusesPreviousTab(t *testing.T) {
	t.Parallel()
	// Tabs: [A, B, C], active=C. Close C → expect B.
	s := newTestSupervisor([]string{"A", "B", "C"}, "C")

	next := s.CloseSession("C")

	assert.Equal(t, "B", next)
	assert.Equal(t, "B", s.activeID)
	assert.Equal(t, []string{"A", "B"}, s.order)
}

func TestCloseSession_FocusesPreviousTab_Middle(t *testing.T) {
	t.Parallel()
	// Tabs: [A, B, C], active=B. Close B → expect A.
	s := newTestSupervisor([]string{"A", "B", "C"}, "B")

	next := s.CloseSession("B")

	assert.Equal(t, "A", next)
	assert.Equal(t, "A", s.activeID)
	assert.Equal(t, []string{"A", "C"}, s.order)
}

func TestCloseSession_FirstTab_FocusesNewFirst(t *testing.T) {
	t.Parallel()
	// Tabs: [A, B, C], active=A. Close A → expect B (new first).
	s := newTestSupervisor([]string{"A", "B", "C"}, "A")

	next := s.CloseSession("A")

	assert.Equal(t, "B", next)
	assert.Equal(t, "B", s.activeID)
	assert.Equal(t, []string{"B", "C"}, s.order)
}

func TestCloseSession_LastRemaining(t *testing.T) {
	t.Parallel()
	// Tabs: [A], active=A. Close A → expect "".
	s := newTestSupervisor([]string{"A"}, "A")

	next := s.CloseSession("A")

	assert.Empty(t, next)
	assert.Empty(t, s.activeID)
	assert.Empty(t, s.order)
}

func TestCloseSession_InactiveTab(t *testing.T) {
	t.Parallel()
	// Tabs: [A, B, C], active=A. Close C → active stays A.
	s := newTestSupervisor([]string{"A", "B", "C"}, "A")

	next := s.CloseSession("C")

	assert.Equal(t, "A", next)
	assert.Equal(t, "A", s.activeID)
	assert.Equal(t, []string{"A", "B"}, s.order)
}

func TestCloseSession_NonExistent(t *testing.T) {
	t.Parallel()
	s := newTestSupervisor([]string{"A", "B"}, "A")

	next := s.CloseSession("Z")

	assert.Equal(t, "A", next)
	assert.Equal(t, []string{"A", "B"}, s.order)
}

func TestCloseSession_TwoTabs_CloseSecond(t *testing.T) {
	t.Parallel()
	// Tabs: [A, B], active=B. Close B → expect A.
	s := newTestSupervisor([]string{"A", "B"}, "B")

	next := s.CloseSession("B")

	assert.Equal(t, "A", next)
	assert.Equal(t, "A", s.activeID)
	assert.Equal(t, []string{"A"}, s.order)
}

// TestSetPendingEvent_RoundTrip verifies that SetPendingEvent stores an event
// for a session and that ConsumePendingEvent retrieves and clears it. This
// is the path used to re-stash a background dialog's originating event when
// the user switches away from the tab that opened it (see #2626).
func TestSetPendingEvent_RoundTrip(t *testing.T) {
	t.Parallel()
	s := newTestSupervisor([]string{"A", "B"}, "A")

	type fakeEvent struct{ id int }
	event := &fakeEvent{id: 7}

	s.SetPendingEvent("A", event)

	assert.Equal(t, event, s.runners["A"].PendingEvent, "event is stored on the runner")
	assert.False(t, s.runners["A"].NeedsAttn, "SetPendingEvent must NOT raise NeedsAttn (the user is already aware)")

	got := s.ConsumePendingEvent("A")
	assert.Equal(t, event, got)
	assert.Nil(t, s.runners["A"].PendingEvent, "event is cleared after consumption")
}

// TestSetPendingEvent_UnknownSession is a no-op (and must not panic).
func TestSetPendingEvent_UnknownSession(t *testing.T) {
	t.Parallel()
	s := newTestSupervisor([]string{"A"}, "A")

	s.SetPendingEvent("does-not-exist", "payload")

	assert.Nil(t, s.runners["A"].PendingEvent, "unrelated runner is untouched")
}

// --- #3217: session-aware stream lifecycle tests ---

// TestIsTopLevelStream covers the isTopLevelStream helper directly.
func TestIsTopLevelStream(t *testing.T) {
	t.Parallel()
	tests := []struct {
		runnerID    string
		evSessionID string
		want        bool
	}{
		{runnerID: "sess-A", evSessionID: "sess-A", want: true},   // exact match → top-level
		{runnerID: "sess-A", evSessionID: "", want: true},         // empty → top-level (backward compat)
		{runnerID: "sess-A", evSessionID: "child-B", want: false}, // different ID → nested
		{runnerID: "sess-A", evSessionID: "sess-B", want: false},  // sibling ID → nested
	}
	for _, tc := range tests {
		got := isTopLevelStream(tc.runnerID, tc.evSessionID)
		assert.Equal(t, tc.want, got,
			"isTopLevelStream(%q, %q)", tc.runnerID, tc.evSessionID)
	}
}

// TestStreamStarted_SubSessionDoesNotDropPendingEvent verifies that a
// StreamStartedEvent carrying a child session ID (nested sub-agent/fork-skill
// stream forwarded through the parent's event channel) does NOT wipe the
// parent runner's pending elicitation event. (#3217)
func TestStreamStarted_SubSessionDoesNotDropPendingEvent(t *testing.T) {
	t.Parallel()
	s := newTestSupervisor([]string{"sess-A", "sess-B"}, "sess-B") // sess-A is background

	elicitation := runtime.ElicitationRequest("confirm?", "form", nil, "", "eid-1", nil, "agent")
	s.runners["sess-A"].PendingEvent = elicitation
	s.runners["sess-A"].NeedsAttn = true
	s.runners["sess-A"].IsRunning = true // already running a top-level turn

	// A nested sub-session stream starts (different SessionID).
	s.handleRuntimeEvent("sess-A", &runtime.StreamStartedEvent{
		Type:      "stream_started",
		SessionID: "child-xyz",
	})

	require.NotNil(t, s.runners["sess-A"].PendingEvent,
		"nested StreamStarted must NOT clear the parent's pending elicitation")
	assert.True(t, s.runners["sess-A"].NeedsAttn,
		"nested StreamStarted must NOT clear NeedsAttn")
	assert.True(t, s.runners["sess-A"].IsRunning,
		"nested StreamStarted must NOT change IsRunning")
}

// TestStreamStopped_SubSessionDoesNotDropPendingEvent verifies that a
// StreamStoppedEvent from a child session does NOT clear the parent's pending
// event, NeedsAttn, or IsRunning. (#3217)
func TestStreamStopped_SubSessionDoesNotDropPendingEvent(t *testing.T) {
	t.Parallel()
	s := newTestSupervisor([]string{"sess-A", "sess-B"}, "sess-B")

	elicitation := runtime.ElicitationRequest("confirm?", "form", nil, "", "eid-2", nil, "agent")
	s.runners["sess-A"].PendingEvent = elicitation
	s.runners["sess-A"].NeedsAttn = true
	s.runners["sess-A"].IsRunning = true

	// A nested sub-session stream stops (different SessionID).
	s.handleRuntimeEvent("sess-A", &runtime.StreamStoppedEvent{
		Type:      "stream_stopped",
		SessionID: "child-xyz",
	})

	require.NotNil(t, s.runners["sess-A"].PendingEvent,
		"nested StreamStopped must NOT clear the parent's pending elicitation")
	assert.True(t, s.runners["sess-A"].NeedsAttn,
		"nested StreamStopped must NOT clear NeedsAttn")
	assert.True(t, s.runners["sess-A"].IsRunning,
		"nested StreamStopped must NOT flip IsRunning to false while parent is still running")
}

// TestStreamStarted_TopLevelSupersedesStalePending verifies that a top-level
// StreamStartedEvent (matching session ID) STILL clears a stale pending event
// — the original intent must be preserved. (#3217)
func TestStreamStarted_TopLevelSupersedesStalePending(t *testing.T) {
	t.Parallel()
	s := newTestSupervisor([]string{"sess-A"}, "sess-A")

	s.runners["sess-A"].PendingEvent = runtime.ElicitationRequest(
		"old?", "form", nil, "", "eid-stale", nil, "agent",
	)
	s.runners["sess-A"].IsRunning = false

	// New top-level turn starts.
	s.handleRuntimeEvent("sess-A", &runtime.StreamStartedEvent{
		Type:      "stream_started",
		SessionID: "sess-A",
	})

	assert.Nil(t, s.runners["sess-A"].PendingEvent,
		"top-level StreamStarted must supersede any stale pending event")
	assert.True(t, s.runners["sess-A"].IsRunning,
		"top-level StreamStarted must set IsRunning")
}

// TestStreamStopped_TopLevelClearsPendingAndNeedsAttn verifies that a
// top-level StreamStoppedEvent correctly clears all three fields. (#3217)
func TestStreamStopped_TopLevelClearsPendingAndNeedsAttn(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		pending any // tea.Msg
	}{
		{
			name:    "elicitation pending",
			pending: runtime.ElicitationRequest("q?", "form", nil, "", "eid-3", nil, "agent"),
		},
		{
			name:    "tool confirmation pending",
			pending: runtime.ToolCallConfirmation(tools.ToolCall{}, tools.Tool{}, "agent", nil),
		},
		{
			name:    "max iterations pending",
			pending: runtime.MaxIterationsReached(10),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestSupervisor([]string{"sess-A"}, "sess-B")
			s.runners["sess-A"].PendingEvent = tc.pending
			s.runners["sess-A"].NeedsAttn = true
			s.runners["sess-A"].IsRunning = true

			s.handleRuntimeEvent("sess-A", &runtime.StreamStoppedEvent{
				Type:      "stream_stopped",
				SessionID: "sess-A",
			})

			assert.Nil(t, s.runners["sess-A"].PendingEvent,
				"top-level StreamStopped must clear PendingEvent")
			assert.False(t, s.runners["sess-A"].NeedsAttn,
				"top-level StreamStopped must clear NeedsAttn")
			assert.False(t, s.runners["sess-A"].IsRunning,
				"top-level StreamStopped must clear IsRunning")
		})
	}
}

// TestStreamStarted_EmptySessionID_TreatedAsTopLevel verifies that an empty
// SessionID is treated as top-level for backward compatibility with emitters
// that omit it. (#3217)
func TestStreamStarted_EmptySessionID_TreatedAsTopLevel(t *testing.T) {
	t.Parallel()
	s := newTestSupervisor([]string{"sess-A"}, "sess-A")

	s.runners["sess-A"].PendingEvent = runtime.ElicitationRequest(
		"old?", "form", nil, "", "eid-old", nil, "agent",
	)

	// Emitter omits SessionID (empty string).
	s.handleRuntimeEvent("sess-A", &runtime.StreamStartedEvent{
		Type:      "stream_started",
		SessionID: "",
	})

	assert.Nil(t, s.runners["sess-A"].PendingEvent,
		"empty SessionID must be treated as top-level and supersede stale pending event")
	assert.True(t, s.runners["sess-A"].IsRunning)
}
