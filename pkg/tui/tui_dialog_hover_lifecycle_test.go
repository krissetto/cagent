package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/tabbar"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestAppDialogCloseHoverDoesNotCrossOpenLifecycle(t *testing.T) {
	m, _ := newTestModel(t)
	m.animationRuntime = animation.NewRuntime()
	m.tabBar = tabbar.New(m.animationRuntime, 0)
	mgr := dialog.New()
	mgr.SetSize(80, 24)
	m.dialogMgr = mgr

	a := dialog.NewExitConfirmationDialog()
	_, _ = m.Update(dialog.OpenDialogMsg{Model: a})
	for m.animationRuntime.ActiveCount() > 0 {
		tick := acceptedRuntimeTick(m.animationRuntime)
		ok := true
		require.True(t, ok)
		_, _ = mgr.Update(tick)
	}
	layers := m.dialogMgr.GetLayers()
	require.Len(t, layers, 1)
	unhoveredA := layers[0].GetContent()
	closeX := layers[0].GetX() + lipgloss.Width(unhoveredA) - styles.DialogStyle.GetBorderRightSize() - 2
	closeY := layers[0].GetY() + styles.DialogStyle.GetBorderTopSize()

	_, _ = m.Update(tea.MouseMotionMsg{X: closeX, Y: closeY})
	hoveredA := m.dialogMgr.GetLayers()[0].GetContent()
	assert.NotEqual(t, unhoveredA, hoveredA, "A close control must be visibly hovered")

	_, closeCmd := m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: closeX, Y: closeY})
	require.NotNil(t, closeCmd)
	for _, msg := range collectMsgs(closeCmd) {
		_, _ = m.Update(msg)
	}
	for m.animationRuntime.ActiveCount() > 0 {
		tick := acceptedRuntimeTick(m.animationRuntime)
		ok := true
		require.True(t, ok)
		_, _ = mgr.Update(tick)
	}
	require.False(t, m.dialogMgr.Open())

	b := dialog.NewHelpDialog(nil)
	_, _ = m.Update(dialog.OpenDialogMsg{Model: b})
	for m.animationRuntime.ActiveCount() > 0 {
		tick := acceptedRuntimeTick(m.animationRuntime)
		ok := true
		require.True(t, ok)
		_, _ = mgr.Update(tick)
	}
	layers = m.dialogMgr.GetLayers()
	require.Len(t, layers, 1)
	freshRuntime := animation.NewRuntime()
	fresh := dialog.New()
	fresh.SetSize(80, 24)
	freshB := dialog.NewHelpDialog(nil)
	_, cmd := fresh.Update(dialog.OpenDialogMsg{Model: freshB})
	_ = cmd
	for freshRuntime.ActiveCount() > 0 {
		tick := acceptedRuntimeTick(freshRuntime)
		ok := true
		require.True(t, ok)
		_, _ = fresh.Update(tick)
	}
	unhoveredB := layers[0].GetContent()
	assert.Equal(t, fresh.GetLayers()[0].GetContent(), unhoveredB,
		"B must render unhovered before any pointer motion in its open lifecycle")

	closeX = layers[0].GetX() + lipgloss.Width(unhoveredB) - styles.DialogStyle.GetBorderRightSize() - 2
	closeY = layers[0].GetY() + styles.DialogStyle.GetBorderTopSize()
	_, _ = m.Update(tea.MouseMotionMsg{X: closeX, Y: closeY})
	hoveredB := m.dialogMgr.GetLayers()[0].GetContent()
	assert.NotEqual(t, unhoveredB, hoveredB, "fresh motion at B close X must activate hover")
}
