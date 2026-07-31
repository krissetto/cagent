package dialog

import (
	"testing"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/plans"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

// TestManagerBackgroundDialog verifies that opening a dialog with a non-nil
// OriginatingEvent marks it as a background dialog, while opening one without
// an event leaves the manager in regular ("modal") mode.
func TestManagerBackgroundDialog(t *testing.T) {
	t.Parallel()

	mgr := New().(*manager)

	assert.False(t, mgr.Open(), "manager starts empty")
	assert.False(t, mgr.TopIsBackground(), "empty manager has no background dialog")
	assert.Nil(t, mgr.TopBackgroundEvent())
	assert.Nil(t, mgr.TopDialog(), "empty manager has no top dialog")

	// Open a regular (modal) dialog without an event.
	modal := NewExitConfirmationDialog()
	mgr.handleOpen(OpenDialogMsg{
		Model: modal,
	})
	assert.True(t, mgr.Open())
	assert.False(t, mgr.TopIsBackground(), "dialog opened without OriginatingEvent must NOT be background")
	assert.Nil(t, mgr.TopBackgroundEvent())
	assert.Same(t, modal, mgr.TopDialog(), "TopDialog returns the modal instance")

	// Stack a background dialog (i.e. one carrying an originating event) on top.
	type fakeEvent struct{ id int }
	event := &fakeEvent{id: 42}
	bg := NewElicitationDialog("Pick a value", nil, nil, "")
	mgr.handleOpen(OpenDialogMsg{
		Model:            bg,
		OriginatingEvent: event,
	})
	assert.True(t, mgr.TopIsBackground(), "dialog opened with OriginatingEvent IS background")
	assert.Same(t, event, mgr.TopBackgroundEvent(), "TopBackgroundEvent returns the originating event")
	assert.Same(t, bg, mgr.TopDialog(), "TopDialog returns the background instance")

	// Closing the top reveals the modal dialog underneath.
	mgr.handleClose()
	assert.True(t, mgr.Open(), "manager still has the modal dialog underneath")
	assert.False(t, mgr.TopIsBackground(), "underneath is the modal dialog, not background")
	assert.Nil(t, mgr.TopBackgroundEvent())
	assert.Same(t, modal, mgr.TopDialog())

	// Closing again empties the stack.
	mgr.handleClose()
	assert.False(t, mgr.Open())
	assert.False(t, mgr.TopIsBackground())
	assert.Nil(t, mgr.TopBackgroundEvent())
	assert.Nil(t, mgr.TopDialog())
}

// TestManagerHasDialog verifies HasDialog sees the whole stack, including
// dialogs buried under the topmost one, unlike TopDialog.
func TestManagerHasDialog(t *testing.T) {
	t.Parallel()

	mgr := New().(*manager)
	isExit := func(d Dialog) bool {
		_, ok := d.(*exitConfirmationDialog)
		return ok
	}

	assert.False(t, mgr.HasDialog(isExit), "empty manager matches nothing")

	mgr.handleOpen(OpenDialogMsg{Model: NewExitConfirmationDialog()})
	assert.True(t, mgr.HasDialog(isExit))

	// Bury it under another dialog: TopDialog no longer sees it, HasDialog does.
	mgr.handleOpen(OpenDialogMsg{Model: NewHelpDialog(nil)})
	_, topIsExit := mgr.TopDialog().(*exitConfirmationDialog)
	require.False(t, topIsExit)
	assert.True(t, mgr.HasDialog(isExit), "a buried dialog must still be found")

	assert.False(t, mgr.HasDialog(func(Dialog) bool { return false }))
}

// TestManagerClosePlanDetail verifies the targeted plan-detail close is
// idempotent: it pops the topmost dialog only when it is the detail for
// exactly the given ref, so duplicates, wrong refs, another dialog on top,
// or an already-closed detail never pop the wrong dialog.
func TestManagerClosePlanDetail(t *testing.T) {
	t.Parallel()

	ref := plans.SharedRef("release")
	newDetail := func() Dialog {
		return NewPlanDetailDialog(plans.Plan{Scope: plans.ScopeShared, Name: "release"})
	}

	t.Run("closes the matching top detail once, duplicates are no-ops", func(t *testing.T) {
		t.Parallel()
		mgr := New().(*manager)
		browser := NewPlanBrowserDialog(plans.ListResult{})
		mgr.handleOpen(OpenDialogMsg{Model: browser})
		mgr.handleOpen(OpenDialogMsg{Model: newDetail()})

		mgr.Update(ClosePlanDetailMsg{Ref: ref})
		require.Same(t, browser, mgr.TopDialog(), "the detail closes, the browser surfaces")

		// The duplicated close arrives after the detail already closed.
		mgr.Update(ClosePlanDetailMsg{Ref: ref})
		assert.Same(t, browser, mgr.TopDialog(), "a duplicate close must not pop the browser")

		mgr.handleClose()
		mgr.Update(ClosePlanDetailMsg{Ref: ref})
		assert.False(t, mgr.Open(), "a close on an empty stack is a no-op")
	})

	t.Run("a detail showing another plan is left alone", func(t *testing.T) {
		t.Parallel()
		mgr := New().(*manager)
		other := NewPlanDetailDialog(plans.Plan{Scope: plans.ScopeShared, Name: "other"})
		mgr.handleOpen(OpenDialogMsg{Model: other})

		mgr.Update(ClosePlanDetailMsg{Ref: ref})
		assert.Same(t, other, mgr.TopDialog(), "a close for a different ref is a no-op")
	})

	t.Run("a non-detail dialog on top is left alone", func(t *testing.T) {
		t.Parallel()
		mgr := New().(*manager)
		mgr.handleOpen(OpenDialogMsg{Model: newDetail()})
		help := NewHelpDialog(nil)
		mgr.handleOpen(OpenDialogMsg{Model: help})

		mgr.Update(ClosePlanDetailMsg{Ref: ref})
		assert.Same(t, help, mgr.TopDialog(), "the covering dialog must not be popped")
		assert.True(t, mgr.HasDialog(func(d Dialog) bool {
			viewer, ok := d.(PlanDetailViewer)
			return ok && viewer.PlanRef() == ref
		}), "the buried detail stays open")
	})
}

type viewCountingDialog struct {
	BaseDialog

	viewCalls int
}

func (d *viewCountingDialog) Init() tea.Cmd { return nil }

func (d *viewCountingDialog) Update(tea.Msg) (layout.Model, tea.Cmd) {
	return d, nil
}

func (d *viewCountingDialog) View() string {
	d.viewCalls++
	return "dialog"
}

func (d *viewCountingDialog) Position() (int, int) { return 0, 0 }

func TestManagerUpdateDoesNotRenderDialog(t *testing.T) {
	t.Parallel()

	d := &viewCountingDialog{}
	mgr := New().(*manager)
	_, _ = mgr.handleOpen(OpenDialogMsg{Model: d})
	calls := d.viewCalls

	_, _ = mgr.Update(messages.WheelCoalescedMsg{Delta: -1, X: 0, Y: 0})
	require.Equal(t, calls, d.viewCalls, "dialog Update speculatively called View")
}

func TestManagerVisualGenerationTracksOnlyVisibleDialogChanges(t *testing.T) {
	t.Parallel()

	bindings := make([]key.Binding, 80)
	for i := range bindings {
		bindings[i] = key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "action"))
	}

	mgr := New().(*manager)
	mgr.SetSize(80, 20)
	_, _ = mgr.handleOpen(OpenDialogMsg{Model: NewHelpDialog(bindings)})
	_ = mgr.GetLayers()
	before := mgr.VisualGeneration()

	_, _ = mgr.Update(messages.WheelCoalescedMsg{Delta: -1, X: 40, Y: 10})
	require.Equal(t, before, mgr.VisualGeneration(), "top-boundary wheel changed the visual generation")

	_, _ = mgr.Update(messages.WheelCoalescedMsg{Delta: 1, X: 40, Y: 10})
	require.Greater(t, mgr.VisualGeneration(), before, "effective dialog scroll did not change the visual generation")
}
