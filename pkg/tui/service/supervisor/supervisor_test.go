package supervisor

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
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
	// Tabs: [A, B, C], active=C. Close C → expect B.
	s := newTestSupervisor([]string{"A", "B", "C"}, "C")

	next := s.CloseSession("C")

	assert.Equal(t, "B", next)
	assert.Equal(t, "B", s.activeID)
	assert.Equal(t, []string{"A", "B"}, s.order)
}

func TestCloseSession_FocusesPreviousTab_Middle(t *testing.T) {
	// Tabs: [A, B, C], active=B. Close B → expect A.
	s := newTestSupervisor([]string{"A", "B", "C"}, "B")

	next := s.CloseSession("B")

	assert.Equal(t, "A", next)
	assert.Equal(t, "A", s.activeID)
	assert.Equal(t, []string{"A", "C"}, s.order)
}

func TestCloseSession_FirstTab_FocusesNewFirst(t *testing.T) {
	// Tabs: [A, B, C], active=A. Close A → expect B (new first).
	s := newTestSupervisor([]string{"A", "B", "C"}, "A")

	next := s.CloseSession("A")

	assert.Equal(t, "B", next)
	assert.Equal(t, "B", s.activeID)
	assert.Equal(t, []string{"B", "C"}, s.order)
}

func TestCloseSession_LastRemaining(t *testing.T) {
	// Tabs: [A], active=A. Close A → expect "".
	s := newTestSupervisor([]string{"A"}, "A")

	next := s.CloseSession("A")

	assert.Empty(t, next)
	assert.Empty(t, s.activeID)
	assert.Empty(t, s.order)
}

func TestCloseSession_InactiveTab(t *testing.T) {
	// Tabs: [A, B, C], active=A. Close C → active stays A.
	s := newTestSupervisor([]string{"A", "B", "C"}, "A")

	next := s.CloseSession("C")

	assert.Equal(t, "A", next)
	assert.Equal(t, "A", s.activeID)
	assert.Equal(t, []string{"A", "B"}, s.order)
}

func TestCloseSession_NonExistent(t *testing.T) {
	s := newTestSupervisor([]string{"A", "B"}, "A")

	next := s.CloseSession("Z")

	assert.Equal(t, "A", next)
	assert.Equal(t, []string{"A", "B"}, s.order)
}

func TestHandleRuntimeEvent_TurnEventsDoNotAffectIsRunning(t *testing.T) {
	// Verify TurnStarted/TurnEnded are deliberately no-ops for supervisor's
	// IsRunning flag — only StreamStarted/Stopped own session-lifetime tab state.
	//
	// Use a raw Supervisor with unopened programReady gate so that the
	// subscribe goroutine from AddSession blocks before hitting nil app.
	// We exercise handleRuntimeEvent directly instead.
	s := New(nil)
	s.runners["owned-1"] = &SessionRunner{ID: "owned-1", SessionID: "owned-1", Kind: SessionKindOwned}
	s.order = append(s.order, "owned-1")
	s.activeID = "owned-1"

	// Simulate stream start.
	s.handleRuntimeEvent("owned-1", runtime.StreamStarted("owned-1", "root"))
	runner := s.GetRunner("owned-1")
	require.NotNil(t, runner)
	assert.True(t, runner.IsRunning, "IsRunning should be true after StreamStarted")

	// TurnStarted/TurnEnded must not change IsRunning.
	s.handleRuntimeEvent("owned-1", runtime.TurnStarted("owned-1", "root"))
	assert.True(t, runner.IsRunning, "IsRunning must stay true after TurnStarted")

	s.handleRuntimeEvent("owned-1", runtime.TurnEnded("owned-1", "root"))
	assert.True(t, runner.IsRunning, "IsRunning must stay true after TurnEnded between turns")

	// Stream end still clears IsRunning.
	s.handleRuntimeEvent("owned-1", runtime.StreamStopped("owned-1", "root"))
	assert.False(t, runner.IsRunning, "IsRunning must be false after StreamStopped")
}

func TestCloseSession_TwoTabs_CloseSecond(t *testing.T) {
	// Tabs: [A, B], active=B. Close B → expect A.
	s := newTestSupervisor([]string{"A", "B"}, "B")

	next := s.CloseSession("B")

	assert.Equal(t, "A", next)
	assert.Equal(t, "A", s.activeID)
	assert.Equal(t, []string{"A"}, s.order)
}
