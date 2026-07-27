package input

import (
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/messages"
)

func TestPointerControllerCoalescesMotionToLatest(t *testing.T) {
	controller := NewPointerController()
	sent := make(chan tea.Msg, 1)
	controller.SetSender(func(msg tea.Msg) { sent <- msg })

	require.Nil(t, controller.Filter(tea.MouseMotionMsg{X: 1, Y: 2}))
	require.Nil(t, controller.Filter(tea.MouseMotionMsg{X: 8, Y: 9}))

	select {
	case raw := <-sent:
		msg := raw.(messages.PointerUpdateMsg)
		require.NotNil(t, msg.Motion)
		assert.Equal(t, 8, msg.Motion.X)
		assert.Equal(t, 9, msg.Motion.Y)
		assert.False(t, msg.Wheel)
	case <-time.After(200 * time.Millisecond):
		t.Fatal("pointer update was not flushed")
	}
}

func TestPointerControllerCombinesWheelAndSuppressesMotion(t *testing.T) {
	controller := NewPointerController()
	sent := make(chan tea.Msg, 1)
	controller.SetSender(func(msg tea.Msg) { sent <- msg })

	require.Nil(t, controller.Filter(tea.MouseMotionMsg{X: 1, Y: 2}))
	require.Nil(t, controller.Filter(tea.MouseWheelMsg{X: 10, Y: 11, Button: tea.MouseWheelDown}))
	require.Nil(t, controller.Filter(tea.MouseMotionMsg{X: 20, Y: 21}))
	require.Nil(t, controller.Filter(tea.MouseWheelMsg{X: 12, Y: 13, Button: tea.MouseWheelDown}))

	select {
	case raw := <-sent:
		msg := raw.(messages.PointerUpdateMsg)
		assert.True(t, msg.Wheel)
		assert.Equal(t, 2, msg.WheelDelta)
		assert.Equal(t, 12, msg.X)
		assert.Equal(t, 13, msg.Y)
		assert.Nil(t, msg.Motion)
	case <-time.After(200 * time.Millisecond):
		t.Fatal("pointer update was not flushed")
	}
}

func TestPointerControllerPreservesBoundariesAndDragMotion(t *testing.T) {
	controller := NewPointerController()
	require.Nil(t, controller.Filter(tea.MouseMotionMsg{X: 3, Y: 4}))

	raw := controller.Filter(tea.MouseClickMsg{X: 5, Y: 6, Button: tea.MouseLeft})
	boundary, ok := raw.(messages.PointerBoundaryMsg)
	require.True(t, ok)
	require.NotNil(t, boundary.Pending.Motion)
	assert.Equal(t, 3, boundary.Pending.Motion.X)
	assert.IsType(t, tea.MouseClickMsg{}, boundary.Event)

	drag := tea.MouseMotionMsg{X: 7, Y: 8}
	assert.Equal(t, drag, controller.Filter(drag))
	assert.IsType(t, tea.MouseReleaseMsg{}, controller.Filter(tea.MouseReleaseMsg{X: 7, Y: 8, Button: tea.MouseLeft}))

	require.Nil(t, controller.Filter(tea.MouseMotionMsg{X: 9, Y: 10}))
}

func TestPointerControllerRaceSafe(t *testing.T) {
	controller := NewPointerController()
	controller.SetSender(func(tea.Msg) {})
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := range 50 {
				controller.Filter(tea.MouseMotionMsg{X: i, Y: j})
				controller.Filter(tea.MouseWheelMsg{X: j, Y: i, Button: tea.MouseWheelDown})
			}
		}(i)
	}
	wg.Wait()
}
