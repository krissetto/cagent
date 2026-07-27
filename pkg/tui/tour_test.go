package tui

import (
	"path/filepath"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/paths"
	tourstate "github.com/docker/docker-agent/pkg/tour"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/tabbar"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

// isolateTourState redirects the config dir so tour-state writes never touch
// the developer's real configuration.
func isolateTourState(t *testing.T) {
	t.Helper()
	paths.SetConfigDir(filepath.Join(t.TempDir(), "config"))
	t.Cleanup(func() { paths.SetConfigDir("") })
}

func TestStartTourMsg_ActivatesTourAndPersists(t *testing.T) {
	isolateTourState(t)

	m, _ := newTestModel(t)

	_, _ = m.Update(messages.StartTourMsg{})

	assert.True(t, m.tour.Active())
	assert.Equal(t, tourstate.StatusDone, tourstate.ReadStatus())
}

func TestStartTourMsg_LeanModeRefuses(t *testing.T) {
	isolateTourState(t)

	m, _ := newTestModel(t)
	m.leanMode = true

	_, cmd := m.Update(messages.StartTourMsg{})

	assert.False(t, m.tour.Active())
	require.NotNil(t, cmd, "lean mode should surface a notification")
	assert.Equal(t, tourstate.StatusUnanswered, tourstate.ReadStatus())
}

func TestTourObservesMessagesThroughUpdate(t *testing.T) {
	isolateTourState(t)

	m, _ := newTestModel(t)
	_, _ = m.Update(messages.StartTourMsg{})
	require.True(t, m.tour.Active())

	// A user message flowing through Update completes the first step: the
	// tour schedules its celebration tick (the mock chat page itself
	// produces no command, so a non-nil command can only come from the
	// tour's observation).
	_, cmd := m.Update(messages.SendMsg{Content: "hello"})
	require.NotNil(t, cmd)

	// While celebrating, further matches schedule nothing new.
	_, cmd = m.Update(messages.SendMsg{Content: "hello again"})
	assert.Nil(t, cmd)
}

func TestTourKeys(t *testing.T) {
	isolateTourState(t)

	t.Run("esc quits the tour", func(t *testing.T) {
		m, _ := newTestModel(t)
		m.tabBar = tabbar.New(animation.NewRuntime(), 0)
		_, _ = m.Update(messages.StartTourMsg{})
		require.True(t, m.tour.Active())

		_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})

		assert.False(t, m.tour.Active())
		require.NotNil(t, cmd)
	})

	t.Run("enter on empty editor advances", func(t *testing.T) {
		m, _ := newTestModel(t)
		m.tabBar = tabbar.New(animation.NewRuntime(), 0)
		m.focusedPanel = PanelEditor
		_, _ = m.Update(messages.StartTourMsg{})
		require.True(t, m.tour.Active())

		_, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})

		assert.True(t, m.tour.Active(), "advancing does not end the tour")
	})
}

func TestTourOfferResult_NeverPersists(t *testing.T) {
	isolateTourState(t)

	m, _ := newTestModel(t)

	_, cmd := m.Update(dialog.TourOfferResultMsg{Choice: dialog.TourOfferNever})

	require.NotNil(t, cmd)
	assert.False(t, m.tour.Active())
	assert.Equal(t, tourstate.StatusNever, tourstate.ReadStatus())
}

func TestTourOfferResult_LaterKeepsStateUnanswered(t *testing.T) {
	isolateTourState(t)

	m, _ := newTestModel(t)

	_, cmd := m.Update(dialog.TourOfferResultMsg{Choice: dialog.TourOfferLater})

	require.NotNil(t, cmd)
	assert.False(t, m.tour.Active())
	assert.Equal(t, tourstate.StatusUnanswered, tourstate.ReadStatus())
}

func TestTourOfferResult_AcceptedStartsTour(t *testing.T) {
	isolateTourState(t)

	m, _ := newTestModel(t)

	_, _ = m.Update(dialog.TourOfferResultMsg{Choice: dialog.TourOfferAccepted})

	assert.True(t, m.tour.Active())
	assert.Equal(t, tourstate.StatusDone, tourstate.ReadStatus())
}
