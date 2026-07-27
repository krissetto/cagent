package tui

import (
	"os"
	"path/filepath"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/plans"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools/builtin/plan"
	"github.com/docker/docker-agent/pkg/tools/builtin/sessionplan"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/service/supervisor"
)

// newPlansTestModel wires an appModel around a temp-backed plans service and
// a real session, returning the model, the service, the active session, and
// the session-plans directory for planting files.
func newPlansTestModel(t *testing.T) (*appModel, plans.Service, *session.Session, string) {
	t.Helper()
	m, _ := newTestModel(t)

	sessionDir := t.TempDir()
	svc := plans.NewService(plan.NewFilesystemStorage(t.TempDir()), plans.WithSessionDir(sessionDir))
	WithPlansService(svc)(m)

	sess := session.New()
	m.application = app.New(t.Context(), stubRuntime{}, sess)
	m.sessionState = service.NewSessionState(sess)
	return m, svc, sess, sessionDir
}

func mustCreatePlan(t *testing.T, svc plans.Service, name, content string) plans.Plan {
	t.Helper()
	p, err := svc.Create(t.Context(), plans.CreateRequest{Ref: plans.SharedRef(name), Content: content})
	require.NoError(t, err)
	return p
}

// openPlanBrowser puts a plan browser dialog on the model's dialog stack, as
// if /plans had been run.
func openPlanBrowser(t *testing.T, m *appModel, result plans.ListResult) {
	t.Helper()
	updated, _ := m.dialogMgr.Update(dialog.OpenDialogMsg{Model: dialog.NewPlanBrowserDialog(result)})
	m.dialogMgr = updated.(dialog.Manager)
	require.True(t, m.dialogMgr.Open())
}

func firstOfType[T any](msgs []tea.Msg) (T, bool) {
	for _, msg := range msgs {
		if typed, ok := msg.(T); ok {
			return typed, true
		}
	}
	var zero T
	return zero, false
}

func notificationTexts(msgs []tea.Msg) []string {
	var texts []string
	for _, msg := range msgs {
		if note, ok := msg.(notification.ShowMsg); ok {
			texts = append(texts, note.Text)
		}
	}
	return texts
}

func TestHandleShowPlanBrowser_OpensDialog(t *testing.T) {
	t.Parallel()
	m, svc, _, _ := newPlansTestModel(t)
	mustCreatePlan(t, svc, "release", "content")

	_, cmd := m.Update(messages.ShowPlanBrowserMsg{})
	msgs := collectMsgs(cmd)
	openMsg, ok := firstOfType[dialog.OpenDialogMsg](msgs)
	require.True(t, ok, "/plans must open the browser dialog")

	openMsg.Model.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	assert.Contains(t, openMsg.Model.View(), "release")
}

func TestPlanList_IncludesOnlyActiveSessionPlan(t *testing.T) {
	t.Parallel()
	m, svc, sess, sessionDir := newPlansTestModel(t)
	mustCreatePlan(t, svc, "shared-one", "content")

	// Current session's plan plus a stale plan from another session that
	// must never be enumerated.
	_, err := sessionplan.WriteContent(sessionDir, sess.ID, "my plan")
	require.NoError(t, err)
	staleSess := session.New()
	_, err = sessionplan.WriteContent(sessionDir, staleSess.ID, "stale plan")
	require.NoError(t, err)

	_, cmd := m.Update(messages.RefreshPlansMsg{})
	dataMsg, ok := firstOfType[dialog.PlanBrowserDataMsg](collectMsgs(cmd))
	require.True(t, ok)

	names := make([]string, 0, len(dataMsg.Result.Plans))
	for _, p := range dataMsg.Result.Plans {
		names = append(names, p.Name)
	}
	assert.ElementsMatch(t, []string{sess.ID, "shared-one"}, names,
		"the listing includes the active session's plan and shared plans, never stale session plans")
}

func TestHandleSetPlanStatus_RefreshesAfterWrite(t *testing.T) {
	t.Parallel()
	m, svc, _, _ := newPlansTestModel(t)
	created := mustCreatePlan(t, svc, "release", "content")
	require.Equal(t, 1, *created.Version)

	_, cmd := m.Update(messages.SetPlanStatusMsg{
		Ref:             plans.SharedRef("release"),
		Status:          "done",
		ExpectedVersion: 1,
	})
	msgs := collectMsgs(cmd)

	texts := notificationTexts(msgs)
	require.NotEmpty(t, texts)
	assert.Contains(t, texts[0], "done")

	dataMsg, ok := firstOfType[dialog.PlanBrowserDataMsg](msgs)
	require.True(t, ok, "a successful write must refresh the browser data")
	require.Len(t, dataMsg.Result.Plans, 1)
	assert.Equal(t, "done", dataMsg.Result.Plans[0].Status)
	assert.Equal(t, 2, *dataMsg.Result.Plans[0].Version)

	stored, err := svc.Get(t.Context(), plans.SharedRef("release"))
	require.NoError(t, err)
	assert.Equal(t, "done", stored.Status)
}

func TestHandleSetPlanStatus_StaleConflictPreservesNewerData(t *testing.T) {
	t.Parallel()
	m, svc, _, _ := newPlansTestModel(t)
	mustCreatePlan(t, svc, "release", "content")

	// Someone else moved the plan to v2 with a newer status.
	v1 := 1
	_, err := svc.SetStatus(t.Context(), plans.SetStatusRequest{
		Ref: plans.SharedRef("release"), Status: "newer", ExpectedVersion: &v1,
	})
	require.NoError(t, err)

	_, cmd := m.Update(messages.SetPlanStatusMsg{
		Ref:             plans.SharedRef("release"),
		Status:          "stale-write",
		ExpectedVersion: 1,
	})
	msgs := collectMsgs(cmd)

	texts := notificationTexts(msgs)
	require.NotEmpty(t, texts)
	assert.Contains(t, texts[0], "conflict")
	assert.Contains(t, texts[0], "v2", "the conflict notification must name the current version")

	// The newer content stays intact and is re-read into the dialogs.
	dataMsg, ok := firstOfType[dialog.PlanBrowserDataMsg](msgs)
	require.True(t, ok, "a conflict must refresh so the newer version is visible")
	assert.Equal(t, "newer", dataMsg.Result.Plans[0].Status)

	stored, err := svc.Get(t.Context(), plans.SharedRef("release"))
	require.NoError(t, err)
	assert.Equal(t, "newer", stored.Status, "the stale write must not clobber the newer status")
	assert.Equal(t, 2, *stored.Version)
}

func TestHandleDeletePlan_Semantics(t *testing.T) {
	t.Parallel()
	m, svc, _, _ := newPlansTestModel(t)
	mustCreatePlan(t, svc, "release", "content")

	// Stale guard: delete is refused and the plan survives.
	v1 := 1
	_, err := svc.SetStatus(t.Context(), plans.SetStatusRequest{
		Ref: plans.SharedRef("release"), Status: "moved-on", ExpectedVersion: &v1,
	})
	require.NoError(t, err)

	_, cmd := m.Update(messages.DeletePlanMsg{Ref: plans.SharedRef("release"), ExpectedVersion: 1})
	msgs := collectMsgs(cmd)
	texts := notificationTexts(msgs)
	require.NotEmpty(t, texts)
	assert.Contains(t, texts[0], "conflict")
	_, err = svc.Get(t.Context(), plans.SharedRef("release"))
	require.NoError(t, err, "a stale delete must leave the plan intact")

	// Correct guard: delete succeeds and the refresh drops the row.
	_, cmd = m.Update(messages.DeletePlanMsg{Ref: plans.SharedRef("release"), ExpectedVersion: 2})
	msgs = collectMsgs(cmd)
	texts = notificationTexts(msgs)
	require.NotEmpty(t, texts)
	assert.Contains(t, texts[0], "Deleted")

	dataMsg, ok := firstOfType[dialog.PlanBrowserDataMsg](msgs)
	require.True(t, ok)
	assert.Empty(t, dataMsg.Result.Plans)

	_, err = svc.Get(t.Context(), plans.SharedRef("release"))
	require.Error(t, err)
}

func TestHandleExportPlan_RefusesOverwrite(t *testing.T) {
	m, svc, _, _ := newPlansTestModel(t)
	mustCreatePlan(t, svc, "release", "the content")

	workDir := t.TempDir()
	t.Chdir(workDir)

	_, cmd := m.Update(messages.ExportPlanMsg{Ref: plans.SharedRef("release")})
	msgs := collectMsgs(cmd)
	note, ok := firstOfType[notification.ShowMsg](msgs)
	require.True(t, ok)
	assert.Equal(t, notification.TypeSuccess, note.Type)

	path := filepath.Join(workDir, "release.md")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "the content", string(data))

	// Second export to the same default path must refuse to overwrite.
	require.NoError(t, os.WriteFile(path, []byte("precious local edits"), 0o600))
	_, cmd = m.Update(messages.ExportPlanMsg{Ref: plans.SharedRef("release")})
	msgs = collectMsgs(cmd)
	note, ok = firstOfType[notification.ShowMsg](msgs)
	require.True(t, ok)
	assert.Equal(t, notification.TypeError, note.Type)
	assert.Contains(t, note.Text, "already exists")

	data, err = os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "precious local edits", string(data), "an existing file must never be overwritten")
}

func TestHandleExportPlan_SessionDefaultFilename(t *testing.T) {
	m, _, sess, sessionDir := newPlansTestModel(t)
	_, err := sessionplan.WriteContent(sessionDir, sess.ID, "session body")
	require.NoError(t, err)

	workDir := t.TempDir()
	t.Chdir(workDir)

	_, cmd := m.Update(messages.ExportPlanMsg{Ref: plans.SessionRef(sess.ID)})
	msgs := collectMsgs(cmd)
	note, ok := firstOfType[notification.ShowMsg](msgs)
	require.True(t, ok)
	assert.Equal(t, notification.TypeSuccess, note.Type)

	path := filepath.Join(workDir, "session-plan-"+sess.ID[:8]+".md")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "session body", string(data))
}

func TestHandlePlanEditorClosed_CreatesPlan(t *testing.T) {
	t.Parallel()
	m, svc, _, _ := newPlansTestModel(t)

	draft := filepath.Join(t.TempDir(), "draft.md")
	require.NoError(t, os.WriteFile(draft, []byte("# fresh plan"), 0o600))

	_, cmd := m.Update(planEditorClosedMsg{ref: plans.SharedRef("fresh"), create: true, path: draft})
	msgs := collectMsgs(cmd)
	texts := notificationTexts(msgs)
	require.NotEmpty(t, texts)
	assert.Contains(t, texts[0], "Created")

	stored, err := svc.Get(t.Context(), plans.SharedRef("fresh"))
	require.NoError(t, err)
	assert.Equal(t, "# fresh plan", stored.Content)

	_, err = os.Stat(draft)
	assert.True(t, os.IsNotExist(err), "the draft is removed after a successful write")
}

func TestHandlePlanEditorClosed_EmptyDraftAborts(t *testing.T) {
	t.Parallel()
	m, svc, _, _ := newPlansTestModel(t)

	draft := filepath.Join(t.TempDir(), "draft.md")
	require.NoError(t, os.WriteFile(draft, []byte("  \n \n"), 0o600))

	_, cmd := m.Update(planEditorClosedMsg{ref: plans.SharedRef("fresh"), create: true, path: draft})
	msgs := collectMsgs(cmd)
	texts := notificationTexts(msgs)
	require.NotEmpty(t, texts)
	assert.Contains(t, texts[0], "not created")

	_, err := svc.Get(t.Context(), plans.SharedRef("fresh"))
	require.Error(t, err, "an empty draft must not create a plan")
}

func TestHandlePlanEditorClosed_ConflictKeepsDraftAndNewerContent(t *testing.T) {
	t.Parallel()
	m, svc, _, _ := newPlansTestModel(t)
	mustCreatePlan(t, svc, "release", "v1 content")

	// The plan moved to v2 while the user was editing v1.
	v1 := 1
	_, err := svc.Update(t.Context(), plans.UpdateRequest{
		Ref: plans.SharedRef("release"), Content: "newer content", ExpectedVersion: &v1,
	})
	require.NoError(t, err)

	draft := filepath.Join(t.TempDir(), "draft.md")
	require.NoError(t, os.WriteFile(draft, []byte("stale edited content"), 0o600))

	_, cmd := m.Update(planEditorClosedMsg{ref: plans.SharedRef("release"), expectedVersion: 1, path: draft})
	msgs := collectMsgs(cmd)
	texts := notificationTexts(msgs)
	require.NotEmpty(t, texts)
	assert.Contains(t, texts[0], "conflict")
	assert.Contains(t, texts[0], "v2")
	assert.Contains(t, texts[0], draft, "the notification must point at the kept draft")

	_, err = os.Stat(draft)
	require.NoError(t, err, "the draft must be kept on conflict")

	stored, err := svc.Get(t.Context(), plans.SharedRef("release"))
	require.NoError(t, err)
	assert.Equal(t, "newer content", stored.Content, "the newer content must stay intact")
}

func TestHandleEditPlan_VersionDriftRefreshesInsteadOfEditing(t *testing.T) {
	t.Parallel()
	m, svc, _, _ := newPlansTestModel(t)
	mustCreatePlan(t, svc, "release", "v1 content")
	v1 := 1
	_, err := svc.Update(t.Context(), plans.UpdateRequest{
		Ref: plans.SharedRef("release"), Content: "v2 content", ExpectedVersion: &v1,
	})
	require.NoError(t, err)

	_, cmd := m.Update(messages.EditPlanMsg{Ref: plans.SharedRef("release"), ExpectedVersion: 1})
	msgs := collectMsgs(cmd)
	texts := notificationTexts(msgs)
	require.NotEmpty(t, texts)
	assert.Contains(t, texts[0], "v2")

	_, refreshed := firstOfType[dialog.PlanBrowserDataMsg](msgs)
	assert.True(t, refreshed, "version drift must refresh the data on screen")
}

func TestSessionPlanUpdatedEvent_RefreshesOpenPlanDialogs(t *testing.T) {
	t.Parallel()
	m, _, sess, sessionDir := newPlansTestModel(t)
	openPlanBrowser(t, m, plans.ListResult{Plans: []plans.Plan{}})

	// The agent writes the session plan; the browser must pick it up.
	_, err := sessionplan.WriteContent(sessionDir, sess.ID, "plan body")
	require.NoError(t, err)

	_, cmd := m.Update(runtime.SessionPlanUpdated(sess.ID, "plan body", "", "root"))
	msgs := collectMsgs(cmd)
	dataMsg, ok := firstOfType[dialog.PlanBrowserDataMsg](msgs)
	require.True(t, ok, "an open plan browser must live-refresh on session plan writes")
	require.Len(t, dataMsg.Result.Plans, 1)
	assert.Equal(t, sess.ID, dataMsg.Result.Plans[0].SessionID)
}

func TestSessionPlanUpdatedEvent_NoRefreshWithoutPlanDialog(t *testing.T) {
	t.Parallel()
	m, _, sess, _ := newPlansTestModel(t)

	_, cmd := m.Update(runtime.SessionPlanUpdated(sess.ID, "plan body", "", "root"))
	_, refreshed := firstOfType[dialog.PlanBrowserDataMsg](collectMsgs(cmd))
	assert.False(t, refreshed, "no plan dialog open, nothing to refresh")
}

func TestPlanChangedEvent_RefreshesOpenPlanDialogs(t *testing.T) {
	t.Parallel()
	m, svc, _, _ := newPlansTestModel(t)
	mustCreatePlan(t, svc, "release", "content")
	openPlanBrowser(t, m, plans.ListResult{Plans: []plans.Plan{}})

	_, cmd := m.Update(runtime.PlanChanged("shared", "release", plan.ChangeActionWrite, 1, "root"))
	msgs := collectMsgs(cmd)
	dataMsg, ok := firstOfType[dialog.PlanBrowserDataMsg](msgs)
	require.True(t, ok, "an open plan browser must live-refresh on shared plan mutations")
	require.Len(t, dataMsg.Result.Plans, 1)
	assert.Equal(t, "release", dataMsg.Result.Plans[0].Name)
}

func TestPlanChangedEvent_BackgroundSessionStillRefreshes(t *testing.T) {
	t.Parallel()
	m, svc, _, _ := newPlansTestModel(t)
	mustCreatePlan(t, svc, "release", "content")

	sv := supervisor.New(nil)
	activeID := sv.AddSession(t.Context(), nil, session.New(), "", nil)
	backgroundID := sv.AddSession(t.Context(), nil, session.New(), "", nil)
	m.supervisor = sv
	require.Equal(t, activeID, sv.ActiveID())
	m.chatPages[backgroundID] = &mockChatPage{}

	openPlanBrowser(t, m, plans.ListResult{Plans: []plans.Plan{}})

	_, cmd := m.Update(messages.RoutedMsg{
		SessionID: backgroundID,
		Inner:     runtime.PlanChanged("shared", "release", plan.ChangeActionStatus, 2, "bg-agent"),
	})
	_, ok := firstOfType[dialog.PlanBrowserDataMsg](collectMsgs(cmd))
	assert.True(t, ok, "shared plans are scope-global: background mutations refresh the open browser")
}

func TestLeanModeDropsPlanBrowser(t *testing.T) {
	t.Parallel()
	// Lean mode short-circuits before any handler runs, so no plans service
	// or application is needed.
	m, _ := newTestModel(t)
	m.leanMode = true

	_, cmd := m.Update(messages.ShowPlanBrowserMsg{})
	assert.Nil(t, cmd, "lean mode has no overlays; /plans is dropped like /settings")
}
