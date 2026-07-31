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

func TestMouseCoalescerCoalescesMotionToLatest(t *testing.T) {
	mouseCoalescer := NewMouseCoalescer()
	sent := make(chan tea.Msg, 1)
	mouseCoalescer.SetSender(func(msg tea.Msg) { sent <- msg })

	require.Nil(t, mouseCoalescer.Filter(tea.MouseMotionMsg{X: 1, Y: 2}))
	require.Nil(t, mouseCoalescer.Filter(tea.MouseMotionMsg{X: 8, Y: 9}))

	select {
	case emitted := <-sent:
		msg := emitted.(messages.PointerUpdateMsg)
		require.NotNil(t, msg.Motion)
		assert.Equal(t, 8, msg.Motion.X)
		assert.Equal(t, 9, msg.Motion.Y)
		assert.False(t, msg.HasWheel)
	case <-time.After(200 * time.Millisecond):
		t.Fatal("pointer update was not flushed")
	}
}

func TestMouseCoalescerCombinesWheelAndSuppressesMotion(t *testing.T) {
	mouseCoalescer := NewMouseCoalescer()
	sent := make(chan tea.Msg, 1)
	mouseCoalescer.SetSender(func(msg tea.Msg) { sent <- msg })

	require.Nil(t, mouseCoalescer.Filter(tea.MouseMotionMsg{X: 1, Y: 2}))
	require.Nil(t, mouseCoalescer.Filter(tea.MouseWheelMsg{X: 10, Y: 11, Button: tea.MouseWheelDown}))
	require.Nil(t, mouseCoalescer.Filter(tea.MouseMotionMsg{X: 20, Y: 21}))
	require.Nil(t, mouseCoalescer.Filter(tea.MouseWheelMsg{X: 12, Y: 13, Button: tea.MouseWheelDown}))

	select {
	case emitted := <-sent:
		msg := emitted.(messages.PointerUpdateMsg)
		assert.True(t, msg.HasWheel)
		assert.Equal(t, 2, msg.WheelDelta)
		assert.Equal(t, 12, msg.X)
		assert.Equal(t, 13, msg.Y)
		assert.Nil(t, msg.Motion)
	case <-time.After(200 * time.Millisecond):
		t.Fatal("pointer update was not flushed")
	}
}

func TestMouseCoalescerCoalescesDragMotionAndFlushesBeforeRelease(t *testing.T) {
	mouseCoalescer := NewMouseCoalescer()

	click := tea.MouseClickMsg{X: 5, Y: 6, Button: tea.MouseLeft}
	assert.Equal(t, click, mouseCoalescer.Filter(click))

	require.Nil(t, mouseCoalescer.Filter(tea.MouseMotionMsg{X: 7, Y: 8, Button: tea.MouseLeft}))
	require.Nil(t, mouseCoalescer.Filter(tea.MouseMotionMsg{X: 9, Y: 10, Button: tea.MouseLeft}))

	release := tea.MouseReleaseMsg{X: 9, Y: 10, Button: tea.MouseLeft}
	filtered := mouseCoalescer.Filter(release)
	boundary, ok := filtered.(messages.PointerBoundaryMsg)
	require.True(t, ok)
	require.NotNil(t, boundary.Pending.Motion)
	assert.Equal(t, 9, boundary.Pending.Motion.X)
	assert.Equal(t, 10, boundary.Pending.Motion.Y)
	assert.Equal(t, tea.MouseLeft, boundary.Pending.Motion.Button)
	assert.Equal(t, release, boundary.Event)
}

func TestMouseCoalescerClickFlushesPendingMotion(t *testing.T) {
	mouseCoalescer := NewMouseCoalescer()
	sent := make(chan tea.Msg, 1)
	mouseCoalescer.SetSender(func(msg tea.Msg) { sent <- msg })
	require.Nil(t, mouseCoalescer.Filter(tea.MouseMotionMsg{X: 3, Y: 4}))

	click := tea.MouseClickMsg{X: 5, Y: 6, Button: tea.MouseLeft}
	filtered := mouseCoalescer.Filter(click)
	boundary, ok := filtered.(messages.PointerBoundaryMsg)
	require.True(t, ok)
	require.NotNil(t, boundary.Pending.Motion)
	assert.Equal(t, 3, boundary.Pending.Motion.X)
	assert.Equal(t, click, boundary.Event)

	select {
	case msg := <-sent:
		t.Fatalf("stale timer sent an update after the click: %#v", msg)
	case <-time.After(2 * mouseFlushInterval):
	}
}

func TestMouseCoalescerReleaseFlushesPendingWheel(t *testing.T) {
	mouseCoalescer := NewMouseCoalescer()
	require.Nil(t, mouseCoalescer.Filter(tea.MouseWheelMsg{X: 11, Y: 12, Button: tea.MouseWheelDown}))

	release := tea.MouseReleaseMsg{X: 11, Y: 12, Button: tea.MouseLeft}
	filtered := mouseCoalescer.Filter(release)
	boundary, ok := filtered.(messages.PointerBoundaryMsg)
	require.True(t, ok)
	assert.True(t, boundary.Pending.HasWheel)
	assert.Equal(t, 1, boundary.Pending.WheelDelta)
	assert.Equal(t, 11, boundary.Pending.X)
	assert.Equal(t, 12, boundary.Pending.Y)
	assert.Equal(t, release, boundary.Event)
}

func TestMouseCoalescerRaceSafe(t *testing.T) {
	mouseCoalescer := NewMouseCoalescer()
	mouseCoalescer.SetSender(func(tea.Msg) {})
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := range 50 {
				mouseCoalescer.Filter(tea.MouseMotionMsg{X: i, Y: j})
				mouseCoalescer.Filter(tea.MouseWheelMsg{X: j, Y: i, Button: tea.MouseWheelDown})
			}
		}(i)
	}
	wg.Wait()
}
