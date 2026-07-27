package dialog

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/plans"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func letterKey(r rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: r, Text: string(r)}
}

// testPlanListing builds a listing with the current session's plan first
// (the service's ordering) and two shared plans.
func testPlanListing() plans.ListResult {
	now := time.Now().UTC()
	return plans.ListResult{
		Plans: []plans.Plan{
			{
				Scope:     plans.ScopeSession,
				Name:      "11112222-3333-4444-5555-666677778888",
				SessionID: "11112222-3333-4444-5555-666677778888",
				UpdatedAt: now.Add(-5 * time.Minute),
			},
			{
				Scope:     plans.ScopeShared,
				Name:      "release",
				Title:     "Release plan for 2025",
				Author:    "architect",
				Status:    "in-progress",
				Version:   new(3),
				UpdatedAt: now.Add(-2 * time.Hour),
			},
			{
				Scope:     plans.ScopeShared,
				Name:      "db-migration",
				Title:     "Database migration",
				Status:    "draft",
				Version:   new(1),
				UpdatedAt: now.Add(-30 * time.Minute),
			},
		},
	}
}

func newTestPlanBrowser(t *testing.T, result plans.ListResult) *planBrowserDialog {
	t.Helper()
	d := NewPlanBrowserDialog(result).(*planBrowserDialog)
	d.Init()
	d.Update(tea.WindowSizeMsg{Width: 100, Height: 50})
	return d
}

func firstMsgOfType[T any](msgs []tea.Msg) (T, bool) {
	for _, msg := range msgs {
		if typed, ok := msg.(T); ok {
			return typed, true
		}
	}
	var zero T
	return zero, false
}

func TestPlanBrowserEmptyList(t *testing.T) {
	t.Parallel()
	d := newTestPlanBrowser(t, plans.ListResult{Plans: []plans.Plan{}})

	view := d.View()
	assert.Contains(t, view, "No plans yet")
	assert.Contains(t, view, "Plans (0)")

	// Actions on an empty list are safe no-ops.
	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	assert.Nil(t, cmd)
	_, cmd = d.Update(letterKey('x'))
	assert.Nil(t, cmd)
	_, cmd = d.Update(letterKey('d'))
	assert.Nil(t, cmd)
}

func TestPlanBrowserRendersScopeIdentityStatusVersionTimeTitle(t *testing.T) {
	t.Parallel()
	d := newTestPlanBrowser(t, testPlanListing())

	view := d.View()
	assert.Contains(t, view, "session", "scope column must name the session scope")
	assert.Contains(t, view, "shared", "scope column must name the shared scope")
	assert.Contains(t, view, "11112222-3333-4444-55", "session plan identity is its session ID")
	assert.Contains(t, view, "release")
	assert.Contains(t, view, "in-progress")
	assert.Contains(t, view, "v3", "shared plan version must be shown")
	assert.Contains(t, view, "2h ago", "updated time must be shown")
	assert.Contains(t, view, "Release plan", "title must be shown")

	// The session plan row renders "-" for its nonexistent version.
	sessionRow := d.renderPlan(d.filtered[0], false, 90)
	assert.Contains(t, sessionRow, "-")
}

func TestPlanBrowserRowsTruncateSafely(t *testing.T) {
	t.Parallel()
	long := plans.Plan{
		Scope:     plans.ScopeShared,
		Name:      strings.Repeat("very-long-name-", 10),
		Title:     strings.Repeat("An extremely long title that cannot fit ", 20),
		Status:    strings.Repeat("status", 10),
		Version:   new(123456),
		UpdatedAt: time.Now(),
	}
	d := newTestPlanBrowser(t, plans.ListResult{Plans: []plans.Plan{long}})

	const width = 80
	row := d.renderPlan(long, false, width)
	assert.LessOrEqual(t, lipgloss.Width(row), width, "row must never overflow the dialog")

	for line := range strings.SplitSeq(d.View(), "\n") {
		assert.LessOrEqual(t, lipgloss.Width(line), 120, "no dialog line may exceed the dialog width")
	}
}

func TestPlanBrowserNavigation(t *testing.T) {
	t.Parallel()
	d := newTestPlanBrowser(t, testPlanListing())

	require.Equal(t, 0, d.selected)
	d.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	assert.Equal(t, 1, d.selected)
	d.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	assert.Equal(t, 2, d.selected)
	d.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	assert.Equal(t, 2, d.selected, "selection stays at the end of the list")
	d.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	assert.Equal(t, 1, d.selected)
}

func TestPlanBrowserFilter(t *testing.T) {
	t.Parallel()
	d := newTestPlanBrowser(t, testPlanListing())

	// "/" enters filter mode; typing narrows the list.
	d.Update(letterKey('/'))
	require.True(t, d.filtering)
	d.Update(letterKey('r'))
	d.Update(letterKey('e'))
	d.Update(letterKey('l'))
	require.Len(t, d.filtered, 1)
	assert.Equal(t, "release", d.filtered[0].Name)

	// Esc leaves filter mode without closing the dialog.
	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	assert.False(t, d.filtering)
	assert.Nil(t, cmd)

	// Outside filter mode, esc closes.
	_, cmd = d.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	msgs := collectMsgs(cmd)
	_, ok := firstMsgOfType[CloseDialogMsg](msgs)
	assert.True(t, ok)
}

func TestPlanBrowserFilterNoMatches(t *testing.T) {
	t.Parallel()
	d := newTestPlanBrowser(t, testPlanListing())

	d.Update(letterKey('/'))
	d.Update(letterKey('z'))
	d.Update(letterKey('z'))
	require.Empty(t, d.filtered)
	assert.Contains(t, d.View(), "No plans match the filter")
}

func TestPlanBrowserEnterOpensDetail(t *testing.T) {
	t.Parallel()
	d := newTestPlanBrowser(t, testPlanListing())

	// Session plan (first row): detail is addressed by session ref.
	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	msg, ok := firstMsgOfType[messages.OpenPlanDetailMsg](collectMsgs(cmd))
	require.True(t, ok, "enter must open the detail dialog, not invoke agent tools")
	assert.Equal(t, plans.SessionRef("11112222-3333-4444-5555-666677778888"), msg.Ref)

	// Shared plan.
	d.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	_, cmd = d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	msg, ok = firstMsgOfType[messages.OpenPlanDetailMsg](collectMsgs(cmd))
	require.True(t, ok)
	assert.Equal(t, plans.SharedRef("release"), msg.Ref)
}

func TestPlanBrowserRefreshAndExportKeys(t *testing.T) {
	t.Parallel()
	d := newTestPlanBrowser(t, testPlanListing())

	_, cmd := d.Update(letterKey('r'))
	_, ok := firstMsgOfType[messages.RefreshPlansMsg](collectMsgs(cmd))
	assert.True(t, ok, "r must request a refresh")

	_, cmd = d.Update(letterKey('x'))
	exportMsg, ok := firstMsgOfType[messages.ExportPlanMsg](collectMsgs(cmd))
	require.True(t, ok, "x must request an export")
	assert.Equal(t, plans.SessionRef("11112222-3333-4444-5555-666677778888"), exportMsg.Ref)
}

func TestPlanBrowserStatusFlow(t *testing.T) {
	t.Parallel()
	d := newTestPlanBrowser(t, testPlanListing())
	d.Update(tea.KeyPressMsg{Code: tea.KeyDown}) // select release, v3

	_, cmd := d.Update(letterKey('s'))
	openMsg, ok := firstMsgOfType[OpenDialogMsg](collectMsgs(cmd))
	require.True(t, ok, "s must open the status input dialog")
	statusDialog, ok := openMsg.Model.(*planStatusDialog)
	require.True(t, ok)
	assert.Equal(t, 3, statusDialog.version, "the displayed version guards the write")
	assert.Contains(t, statusDialog.View(), "release")
	assert.Contains(t, statusDialog.View(), "v3")

	// The input is prefilled with the current status; replace it.
	statusDialog.input.SetValue("done")
	_, cmd = statusDialog.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	msgs := collectMsgs(cmd)
	_, closed := firstMsgOfType[CloseDialogMsg](msgs)
	assert.True(t, closed)
	statusMsg, ok := firstMsgOfType[messages.SetPlanStatusMsg](msgs)
	require.True(t, ok)
	assert.Equal(t, plans.SharedRef("release"), statusMsg.Ref)
	assert.Equal(t, "done", statusMsg.Status)
	assert.Equal(t, 3, statusMsg.ExpectedVersion)
}

func TestPlanBrowserStatusEmptyRejected(t *testing.T) {
	t.Parallel()
	sd := newPlanStatusDialog("release", "", 3).(*planStatusDialog)
	sd.input.SetValue("   ")

	_, cmd := sd.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	msgs := collectMsgs(cmd)
	_, gotStatus := firstMsgOfType[messages.SetPlanStatusMsg](msgs)
	assert.False(t, gotStatus, "an empty status must not be submitted")
	note, ok := firstMsgOfType[notification.ShowMsg](msgs)
	require.True(t, ok)
	assert.Equal(t, notification.TypeError, note.Type)
}

func TestPlanBrowserSessionMutationsUnsupported(t *testing.T) {
	t.Parallel()
	d := newTestPlanBrowser(t, testPlanListing()) // session plan selected

	for _, r := range []rune{'s', 'd', 'e'} {
		_, cmd := d.Update(letterKey(r))
		msgs := collectMsgs(cmd)
		note, ok := firstMsgOfType[notification.ShowMsg](msgs)
		require.True(t, ok, "%c on a session plan must show an explanatory notification", r)
		assert.Contains(t, note.Text, "Session plans")
		_, opened := firstMsgOfType[OpenDialogMsg](msgs)
		assert.False(t, opened, "%c must not open an action dialog for session plans", r)
		_, statusEmitted := firstMsgOfType[messages.SetPlanStatusMsg](msgs)
		assert.False(t, statusEmitted)
		_, deleteEmitted := firstMsgOfType[messages.DeletePlanMsg](msgs)
		assert.False(t, deleteEmitted)
		_, editEmitted := firstMsgOfType[messages.EditPlanMsg](msgs)
		assert.False(t, editEmitted)
	}
}

func TestPlanBrowserDeleteFlow(t *testing.T) {
	t.Parallel()
	d := newTestPlanBrowser(t, testPlanListing())
	d.Update(tea.KeyPressMsg{Code: tea.KeyDown}) // select release, v3

	_, cmd := d.Update(letterKey('d'))
	openMsg, ok := firstMsgOfType[OpenDialogMsg](collectMsgs(cmd))
	require.True(t, ok, "d must open a confirmation dialog")
	confirm, ok := openMsg.Model.(*planDeleteConfirmDialog)
	require.True(t, ok)

	view := confirm.View()
	assert.Contains(t, view, "release", "the confirmation must name the plan")
	assert.Contains(t, view, "version 3", "the confirmation must name the version")

	// N cancels without emitting a delete.
	_, cmd = confirm.Update(letterKey('n'))
	msgs := collectMsgs(cmd)
	_, deleted := firstMsgOfType[messages.DeletePlanMsg](msgs)
	assert.False(t, deleted)
	_, closed := firstMsgOfType[CloseDialogMsg](msgs)
	assert.True(t, closed)

	// Y confirms with the displayed version as the guard.
	_, cmd = confirm.Update(letterKey('y'))
	msgs = collectMsgs(cmd)
	deleteMsg, ok := firstMsgOfType[messages.DeletePlanMsg](msgs)
	require.True(t, ok)
	assert.Equal(t, plans.SharedRef("release"), deleteMsg.Ref)
	assert.Equal(t, 3, deleteMsg.ExpectedVersion)
}

func TestPlanBrowserNewFlow(t *testing.T) {
	t.Parallel()
	d := newTestPlanBrowser(t, testPlanListing())

	_, cmd := d.Update(letterKey('n'))
	openMsg, ok := firstMsgOfType[OpenDialogMsg](collectMsgs(cmd))
	require.True(t, ok, "n must open the name dialog")
	nameDialog, ok := openMsg.Model.(*planNameDialog)
	require.True(t, ok)

	for _, r := range "my-plan" {
		nameDialog.Update(letterKey(r))
	}
	_, cmd = nameDialog.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	msgs := collectMsgs(cmd)
	createMsg, ok := firstMsgOfType[messages.CreatePlanMsg](msgs)
	require.True(t, ok)
	assert.Equal(t, "my-plan", createMsg.Name)
}

func TestPlanBrowserEditKey(t *testing.T) {
	t.Parallel()
	d := newTestPlanBrowser(t, testPlanListing())
	d.Update(tea.KeyPressMsg{Code: tea.KeyDown}) // select release, v3

	_, cmd := d.Update(letterKey('e'))
	editMsg, ok := firstMsgOfType[messages.EditPlanMsg](collectMsgs(cmd))
	require.True(t, ok)
	assert.Equal(t, plans.SharedRef("release"), editMsg.Ref)
	assert.Equal(t, 3, editMsg.ExpectedVersion)
}

func TestPlanBrowserDataMsgReplacesRowsAndKeepsSelection(t *testing.T) {
	t.Parallel()
	d := newTestPlanBrowser(t, testPlanListing())
	d.Update(tea.KeyPressMsg{Code: tea.KeyDown}) // select release

	updated := plans.ListResult{Plans: []plans.Plan{
		{Scope: plans.ScopeShared, Name: "db-migration", Version: new(1)},
		{Scope: plans.ScopeShared, Name: "release", Status: "done", Version: new(4)},
	}}
	d.Update(PlanBrowserDataMsg{Result: updated})

	require.Len(t, d.filtered, 2)
	p, ok := d.selectedPlan()
	require.True(t, ok)
	assert.Equal(t, "release", p.Name, "selection follows the plan identity across refreshes")
	assert.Equal(t, 4, *p.Version)
	assert.NotContains(t, d.View(), "11112222", "removed rows must disappear")
}

func TestPlanBrowserWarningsShown(t *testing.T) {
	t.Parallel()
	result := testPlanListing()
	result.Warnings = []string{`skipped "broken": corrupt`}
	d := newTestPlanBrowser(t, result)

	assert.Contains(t, d.View(), "could not be read")
}

// --- Detail dialog ---

func newTestPlanDetail(t *testing.T, p plans.Plan) *planDetailDialog {
	t.Helper()
	d := NewPlanDetailDialog(p).(*planDetailDialog)
	d.Init()
	d.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	return d
}

func sharedDetailPlan() plans.Plan {
	return plans.Plan{
		Scope:     plans.ScopeShared,
		Name:      "release",
		Title:     "Release plan",
		Author:    "architect",
		Status:    "in-progress",
		Content:   "# Release\n\nStep one.",
		Version:   new(3),
		UpdatedAt: time.Now().Add(-time.Hour),
	}
}

func TestPlanDetailRendersSharedMetadata(t *testing.T) {
	t.Parallel()
	d := newTestPlanDetail(t, sharedDetailPlan())

	view := d.View()
	assert.Contains(t, view, "shared — collaborative, versioned")
	assert.Contains(t, view, "release")
	assert.Contains(t, view, "in-progress")
	assert.Contains(t, view, "v3")
	assert.Contains(t, view, "architect")
	assert.Contains(t, view, "1h ago")
	assert.Contains(t, view, "Step one.")
}

func TestPlanDetailRendersSessionMetadata(t *testing.T) {
	t.Parallel()
	p := plans.Plan{
		Scope:     plans.ScopeSession,
		Name:      "11112222-3333-4444-5555-666677778888",
		SessionID: "11112222-3333-4444-5555-666677778888",
		Content:   "session plan body",
		UpdatedAt: time.Now(),
	}
	d := newTestPlanDetail(t, p)

	view := d.View()
	assert.Contains(t, view, "read-only here", "session scope must be explicit")
	assert.Contains(t, view, "11112222-3333-4444-5555-666677778888")
	assert.Contains(t, view, "session plans have no versions")
	assert.NotContains(t, view, "status", "session detail must not advertise unsupported actions")
	assert.Contains(t, view, "session plan body")
}

func TestPlanDetailScrollsLongContent(t *testing.T) {
	t.Parallel()
	p := sharedDetailPlan()
	var body strings.Builder
	for i := range 200 {
		body.WriteString("line ")
		body.WriteString(strings.Repeat("x", i%10))
		body.WriteString("\n")
	}
	p.Content = body.String()
	d := newTestPlanDetail(t, p)

	before := d.View()
	require.Equal(t, 0, d.scrollview.ScrollOffset())
	d.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	assert.Equal(t, 1, d.scrollview.ScrollOffset(), "down must scroll the content")
	d.Update(tea.KeyPressMsg{Code: tea.KeyPgDown})
	assert.Greater(t, d.scrollview.ScrollOffset(), 1)

	after := d.View()
	assert.Equal(t, lipgloss.Height(before), lipgloss.Height(after), "dialog height must stay stable while scrolling")
}

func TestPlanDetailKeysEmitIntents(t *testing.T) {
	t.Parallel()
	d := newTestPlanDetail(t, sharedDetailPlan())

	_, cmd := d.Update(letterKey('r'))
	_, ok := firstMsgOfType[messages.RefreshPlansMsg](collectMsgs(cmd))
	assert.True(t, ok)

	_, cmd = d.Update(letterKey('x'))
	exportMsg, ok := firstMsgOfType[messages.ExportPlanMsg](collectMsgs(cmd))
	require.True(t, ok)
	assert.Equal(t, plans.SharedRef("release"), exportMsg.Ref)

	_, cmd = d.Update(letterKey('e'))
	editMsg, ok := firstMsgOfType[messages.EditPlanMsg](collectMsgs(cmd))
	require.True(t, ok)
	assert.Equal(t, 3, editMsg.ExpectedVersion)

	_, cmd = d.Update(letterKey('s'))
	openMsg, ok := firstMsgOfType[OpenDialogMsg](collectMsgs(cmd))
	require.True(t, ok)
	_, isStatus := openMsg.Model.(*planStatusDialog)
	assert.True(t, isStatus)

	_, cmd = d.Update(letterKey('d'))
	openMsg, ok = firstMsgOfType[OpenDialogMsg](collectMsgs(cmd))
	require.True(t, ok)
	_, isConfirm := openMsg.Model.(*planDeleteConfirmDialog)
	assert.True(t, isConfirm)

	_, cmd = d.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	_, closed := firstMsgOfType[CloseDialogMsg](collectMsgs(cmd))
	assert.True(t, closed)
}

func TestPlanDetailSessionActionsUnsupported(t *testing.T) {
	t.Parallel()
	p := plans.Plan{
		Scope:     plans.ScopeSession,
		Name:      "sess-1",
		SessionID: "sess-1",
		Content:   "body",
	}
	d := newTestPlanDetail(t, p)

	for _, r := range []rune{'s', 'd', 'e'} {
		_, cmd := d.Update(letterKey(r))
		msgs := collectMsgs(cmd)
		note, ok := firstMsgOfType[notification.ShowMsg](msgs)
		require.True(t, ok, "%c must explain that session plans are read-only", r)
		assert.Contains(t, note.Text, "Session plans")
		_, opened := firstMsgOfType[OpenDialogMsg](msgs)
		assert.False(t, opened)
	}
}

func TestPlanDetailDataMsgAppliesOnlyMatchingPlan(t *testing.T) {
	t.Parallel()
	d := newTestPlanDetail(t, sharedDetailPlan())

	other := sharedDetailPlan()
	other.Name = "other"
	other.Status = "done"
	d.Update(PlanDetailDataMsg{Plan: other})
	assert.Contains(t, d.View(), "in-progress", "data for another plan must be ignored")

	updated := sharedDetailPlan()
	updated.Status = "done"
	updated.Version = new(4)
	updated.Content = "new content"
	d.Update(PlanDetailDataMsg{Plan: updated})
	view := d.View()
	assert.Contains(t, view, "done")
	assert.Contains(t, view, "v4")
	assert.Contains(t, view, "new content")
}

func TestPlanDialogMarkers(t *testing.T) {
	t.Parallel()

	_, isPlanDialog := NewPlanBrowserDialog(plans.ListResult{}).(PlanDialog)
	assert.True(t, isPlanDialog)
	detail := NewPlanDetailDialog(sharedDetailPlan())
	_, isPlanDialog = detail.(PlanDialog)
	assert.True(t, isPlanDialog)

	viewer, ok := detail.(PlanDetailViewer)
	require.True(t, ok)
	assert.Equal(t, plans.SharedRef("release"), viewer.PlanRef())
}
