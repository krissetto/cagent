package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
	openDialog(t, m, dialog.NewPlanBrowserDialog(result))
}

// openDialog pushes d onto the model's dialog stack.
func openDialog(t *testing.T, m *appModel, d dialog.Dialog) {
	t.Helper()
	updated, _ := m.dialogMgr.Update(dialog.OpenDialogMsg{Model: d})
	m.dialogMgr = updated.(dialog.Manager)
	require.True(t, m.dialogMgr.Open())
}

// sizeDialogs gives the dialog manager (and every dialog opened afterwards)
// real dimensions so buried dialogs render meaningful views.
func sizeDialogs(t *testing.T, m *appModel) {
	t.Helper()
	updated, _ := m.dialogMgr.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	m.dialogMgr = updated.(dialog.Manager)
}

// runPlanFlow drives msg through the model like the Bubble Tea runtime
// would: produced commands run on the test goroutine and the typed results
// of asynchronous plan mutations are dispatched back into Update. All other
// produced messages are returned in order.
func runPlanFlow(t *testing.T, m *appModel, msg tea.Msg) []tea.Msg {
	t.Helper()
	var out []tea.Msg
	queue := []tea.Msg{msg}
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		_, cmd := m.Update(next)
		for _, produced := range collectMsgs(cmd) {
			switch produced.(type) {
			case planStatusResultMsg, planDeleteResultMsg, planWriteResultMsg:
				queue = append(queue, produced)
			default:
				out = append(out, produced)
			}
		}
	}
	return out
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

	msgs := runPlanFlow(t, m, messages.SetPlanStatusMsg{
		Ref:             plans.SharedRef("release"),
		Status:          "done",
		ExpectedVersion: 1,
	})

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

	msgs := runPlanFlow(t, m, messages.SetPlanStatusMsg{
		Ref:             plans.SharedRef("release"),
		Status:          "stale-write",
		ExpectedVersion: 1,
	})

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

	msgs := runPlanFlow(t, m, messages.DeletePlanMsg{Ref: plans.SharedRef("release"), ExpectedVersion: 1})
	texts := notificationTexts(msgs)
	require.NotEmpty(t, texts)
	assert.Contains(t, texts[0], "conflict")
	_, err = svc.Get(t.Context(), plans.SharedRef("release"))
	require.NoError(t, err, "a stale delete must leave the plan intact")

	// Correct guard: delete succeeds and the refresh drops the row.
	msgs = runPlanFlow(t, m, messages.DeletePlanMsg{Ref: plans.SharedRef("release"), ExpectedVersion: 2})
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

	msgs := runPlanFlow(t, m, planEditorClosedMsg{ref: plans.SharedRef("fresh"), create: true, path: draft})
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

	msgs := runPlanFlow(t, m, planEditorClosedMsg{ref: plans.SharedRef("fresh"), create: true, path: draft})
	texts := notificationTexts(msgs)
	require.NotEmpty(t, texts)
	assert.Contains(t, texts[0], "not created")

	_, err := svc.Get(t.Context(), plans.SharedRef("fresh"))
	require.Error(t, err, "an empty draft must not create a plan")

	_, err = os.Stat(draft)
	assert.True(t, os.IsNotExist(err), "an empty draft is removed, not kept")
}

// TestHandlePlanEditorClosed_OversizedDraftRefusedBounded proves the draft
// read is bounded: a draft past the plan content cap is refused with an
// actionable notification — detected from the descriptor size, never by
// slurping the file whole — and the draft is preserved for the user to trim.
func TestHandlePlanEditorClosed_OversizedDraftRefusedBounded(t *testing.T) {
	t.Parallel()
	m, svc, _, _ := newPlansTestModel(t)

	draft := filepath.Join(t.TempDir(), "draft.md")
	require.NoError(t, os.WriteFile(draft, make([]byte, plan.MaxPlanContentSize+1), 0o600))

	msgs := runPlanFlow(t, m, planEditorClosedMsg{ref: plans.SharedRef("fresh"), create: true, path: draft})
	texts := notificationTexts(msgs)
	require.NotEmpty(t, texts)
	assert.Contains(t, texts[0], "maximum plan size")
	assert.Contains(t, strings.Join(texts, " "), draft, "the notification must point at the kept draft")

	_, err := svc.Get(t.Context(), plans.SharedRef("fresh"))
	require.Error(t, err, "an oversized draft must not create a plan")
	_, err = os.Stat(draft)
	require.NoError(t, err, "the draft must be kept when it is refused")
}

// TestHandlePlanEditorClosed_NonRegularDraftRejected proves a draft path
// that is not a regular file (here: a directory) is refused on the opened
// descriptor instead of read, with the path preserved.
func TestHandlePlanEditorClosed_NonRegularDraftRejected(t *testing.T) {
	t.Parallel()
	m, svc, _, _ := newPlansTestModel(t)

	draftDir := t.TempDir()
	msgs := runPlanFlow(t, m, planEditorClosedMsg{ref: plans.SharedRef("fresh"), create: true, path: draftDir})
	texts := notificationTexts(msgs)
	require.NotEmpty(t, texts)
	assert.Contains(t, texts[0], "not a regular file")

	_, err := svc.Get(t.Context(), plans.SharedRef("fresh"))
	require.Error(t, err, "nothing may be written from an unreadable draft")
	_, err = os.Stat(draftDir)
	require.NoError(t, err, "the draft path must be preserved on a read failure")
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

	msgs := runPlanFlow(t, m, planEditorClosedMsg{ref: plans.SharedRef("release"), expectedVersion: 1, path: draft})
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

// --- Asynchronous mutations under a wedged lock --------------------------------

// blockingPlansService simulates a wedged cross-process plans lock: every
// mutation blocks until its context expires and reports the deadline as a
// typed storage error, exactly like the real storage's lock acquisition.
// Reads delegate to the embedded real service.
type blockingPlansService struct {
	plans.Service

	mutationsStarted atomic.Int32
}

func (s *blockingPlansService) block(ctx context.Context, op string) error {
	s.mutationsStarted.Add(1)
	<-ctx.Done()
	return &plans.StorageError{Scope: plans.ScopeShared, Op: op, Err: ctx.Err()}
}

func (s *blockingPlansService) Create(ctx context.Context, _ plans.CreateRequest) (plans.Plan, error) {
	return plans.Plan{}, s.block(ctx, "create")
}

func (s *blockingPlansService) Update(ctx context.Context, _ plans.UpdateRequest) (plans.Plan, error) {
	return plans.Plan{}, s.block(ctx, "update")
}

func (s *blockingPlansService) SetStatus(ctx context.Context, _ plans.SetStatusRequest) (plans.Plan, error) {
	return plans.Plan{}, s.block(ctx, "set_status")
}

func (s *blockingPlansService) Delete(ctx context.Context, _ plans.DeleteRequest) error {
	return s.block(ctx, "delete")
}

// TestHandleSetPlanStatus_WedgedLockTimesOutAsynchronously proves the
// mutation never runs inside Update: the event loop stays responsive while
// the write is pending, and a wedged lock surfaces as a bounded, actionable
// timeout notification instead of a freeze.
func TestHandleSetPlanStatus_WedgedLockTimesOutAsynchronously(t *testing.T) {
	t.Parallel()
	m, svc, _, _ := newPlansTestModel(t)
	mustCreatePlan(t, svc, "release", "content")
	blocking := &blockingPlansService{Service: svc}
	WithPlansService(blocking)(m)
	m.planMutationTimeout = 50 * time.Millisecond

	_, cmd := m.Update(messages.SetPlanStatusMsg{
		Ref:             plans.SharedRef("release"),
		Status:          "done",
		ExpectedVersion: 1,
	})
	require.NotNil(t, cmd)
	assert.Zero(t, blocking.mutationsStarted.Load(), "Update must defer the mutation to a command, never run it inline")

	// The TUI stays responsive while the mutation is pending: a read-driven
	// refresh is processed before the mutation command has even run.
	_, refreshCmd := m.Update(messages.RefreshPlansMsg{})
	_, ok := firstOfType[dialog.PlanBrowserDataMsg](collectMsgs(refreshCmd))
	assert.True(t, ok, "reads keep working while a mutation is pending")

	// Running the command blocks on the wedged lock until the bounded
	// timeout fires, then reports back as a typed result message.
	start := time.Now()
	result := cmd()
	elapsed := time.Since(start)
	statusResult, ok := result.(planStatusResultMsg)
	require.True(t, ok, "the command must yield a typed result, got %T", result)
	assert.Equal(t, int32(1), blocking.mutationsStarted.Load())
	assert.GreaterOrEqual(t, elapsed, 40*time.Millisecond, "the command waits for the bounded timeout")
	assert.Less(t, elapsed, time.Second, "the bounded timeout must fire, not the 5s default or never")
	require.Error(t, statusResult.err)
	require.ErrorIs(t, statusResult.err, context.DeadlineExceeded)

	// Dispatching the result yields the actionable notification.
	_, notifyCmd := m.Update(result)
	note, ok := firstOfType[notification.ShowMsg](collectMsgs(notifyCmd))
	require.True(t, ok)
	assert.Equal(t, notification.TypeError, note.Type)
	assert.Contains(t, note.Text, "timed out")
	assert.Contains(t, note.Text, "locked")
}

func TestHandleDeletePlan_WedgedLockTimesOutAsynchronously(t *testing.T) {
	t.Parallel()
	m, svc, _, _ := newPlansTestModel(t)
	mustCreatePlan(t, svc, "release", "content")
	blocking := &blockingPlansService{Service: svc}
	WithPlansService(blocking)(m)
	m.planMutationTimeout = 50 * time.Millisecond

	_, cmd := m.Update(messages.DeletePlanMsg{Ref: plans.SharedRef("release"), ExpectedVersion: 1})
	require.NotNil(t, cmd)
	assert.Zero(t, blocking.mutationsStarted.Load())

	result := cmd()
	deleteResult, ok := result.(planDeleteResultMsg)
	require.True(t, ok, "the command must yield a typed result, got %T", result)
	require.ErrorIs(t, deleteResult.err, context.DeadlineExceeded)

	_, notifyCmd := m.Update(result)
	note, ok := firstOfType[notification.ShowMsg](collectMsgs(notifyCmd))
	require.True(t, ok)
	assert.Equal(t, notification.TypeError, note.Type)
	assert.Contains(t, note.Text, "timed out")

	// The plan survives the timed-out delete.
	_, err := svc.Get(t.Context(), plans.SharedRef("release"))
	require.NoError(t, err)
}

// TestHandlePlanEditorClosed_TimeoutKeepsDraft proves a timed-out
// editor-driven write behaves like any other failed write: the draft file
// survives and the notification points at it.
func TestHandlePlanEditorClosed_TimeoutKeepsDraft(t *testing.T) {
	t.Parallel()
	m, svc, _, _ := newPlansTestModel(t)
	blocking := &blockingPlansService{Service: svc}
	WithPlansService(blocking)(m)
	m.planMutationTimeout = 50 * time.Millisecond

	draft := filepath.Join(t.TempDir(), "draft.md")
	require.NoError(t, os.WriteFile(draft, []byte("drafted content"), 0o600))

	_, cmd := m.Update(planEditorClosedMsg{ref: plans.SharedRef("fresh"), create: true, path: draft})
	require.NotNil(t, cmd)
	assert.Zero(t, blocking.mutationsStarted.Load(), "Update must defer the write to a command")

	result := cmd()
	writeResult, ok := result.(planWriteResultMsg)
	require.True(t, ok, "the command must yield a typed result, got %T", result)
	require.Error(t, writeResult.err)
	require.ErrorIs(t, writeResult.err, context.DeadlineExceeded)

	_, notifyCmd := m.Update(result)
	texts := notificationTexts(collectMsgs(notifyCmd))
	require.NotEmpty(t, texts)
	assert.Contains(t, texts[0], "timed out")
	assert.Contains(t, strings.Join(texts, " "), draft, "the notification must point at the kept draft")

	_, err := os.Stat(draft)
	require.NoError(t, err, "the draft must be kept when the write times out")
}

// --- Live refresh reaches buried plan dialogs -----------------------------------

// TestPlanChangedEvent_RefreshesBuriedPlanDialogs stacks a real non-plan
// dialog on top of the plan browser and detail, fires a PlanChanged event,
// and proves both buried dialogs receive the updated data once the produced
// commands are applied.
func TestPlanChangedEvent_RefreshesBuriedPlanDialogs(t *testing.T) {
	t.Parallel()
	m, svc, _, _ := newPlansTestModel(t)
	mustCreatePlan(t, svc, "release", "v1 content")

	sizeDialogs(t, m)
	browser := dialog.NewPlanBrowserDialog(plans.ListResult{Plans: []plans.Plan{}})
	openDialog(t, m, browser)
	p, err := svc.Get(t.Context(), plans.SharedRef("release"))
	require.NoError(t, err)
	detail := dialog.NewPlanDetailDialog(p)
	openDialog(t, m, detail)
	openDialog(t, m, dialog.NewHelpDialog(nil))

	// An agent moves the plan to v2 while the plan dialogs are buried.
	v1 := 1
	_, err = svc.Update(t.Context(), plans.UpdateRequest{
		Ref: plans.SharedRef("release"), Content: "v2 content", ExpectedVersion: &v1,
	})
	require.NoError(t, err)

	_, cmd := m.Update(runtime.PlanChanged("shared", "release", plan.ChangeActionWrite, 2, "root"))
	msgs := collectMsgs(cmd)

	listMsg, ok := firstOfType[dialog.PlanBrowserDataMsg](msgs)
	require.True(t, ok, "a buried browser must still trigger a list refresh")
	require.Len(t, listMsg.Result.Plans, 1)

	detailMsg, ok := firstOfType[dialog.PlanDetailDataMsg](msgs)
	require.True(t, ok, "a buried detail must be refreshed by its exact ref")
	assert.Equal(t, "release", detailMsg.Plan.Name)
	assert.Equal(t, "v2 content", detailMsg.Plan.Content)

	// Apply the broadcasts and prove the buried instances updated.
	_, _ = m.Update(listMsg)
	_, _ = m.Update(detailMsg)
	assert.Contains(t, browser.View(), "release", "the buried browser must render the refreshed rows")
	assert.Contains(t, detail.View(), "v2 content", "the buried detail must render the refreshed plan")
}

func TestSessionPlanUpdatedEvent_RefreshesBuriedBrowser(t *testing.T) {
	t.Parallel()
	m, _, sess, sessionDir := newPlansTestModel(t)

	sizeDialogs(t, m)
	browser := dialog.NewPlanBrowserDialog(plans.ListResult{Plans: []plans.Plan{}})
	openDialog(t, m, browser)
	openDialog(t, m, dialog.NewHelpDialog(nil)) // real non-plan dialog on top

	_, err := sessionplan.WriteContent(sessionDir, sess.ID, "plan body")
	require.NoError(t, err)

	_, cmd := m.Update(runtime.SessionPlanUpdated(sess.ID, "plan body", "", "root"))
	msgs := collectMsgs(cmd)
	dataMsg, ok := firstOfType[dialog.PlanBrowserDataMsg](msgs)
	require.True(t, ok, "a buried plan browser must still live-refresh on session plan writes")
	require.Len(t, dataMsg.Result.Plans, 1)

	// Apply the broadcast: the buried browser instance receives the rows.
	_, _ = m.Update(dataMsg)
	assert.Contains(t, browser.View(), sess.ID[:8], "the buried browser must render the refreshed rows")
}

// failingGetPlansService wraps a real service and fails Get for one exact
// ref with a configured error, so tests drive detail-refresh failures
// deterministically instead of through fragile filesystem state.
type failingGetPlansService struct {
	plans.Service

	failRef plans.Ref
	err     error
}

func (s *failingGetPlansService) Get(ctx context.Context, ref plans.Ref) (plans.Plan, error) {
	if s.err != nil && ref == s.failRef {
		return plans.Plan{}, s.err
	}
	return s.Service.Get(ctx, ref)
}

// TestPlanRefresh_BuriedDetailSuppressesErrorsUntilSurfaced proves a detail
// dialog whose plan can no longer be read does not spam notifications while
// it is buried under another dialog, whatever the error: refreshes skip it
// silently (a buried detail cannot be closed without popping the wrong
// dialog), and exactly one notification — plus a close only for a plan that
// disappeared — happens once it surfaces and the next refresh runs.
func TestPlanRefresh_BuriedDetailSuppressesErrorsUntilSurfaced(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		getErr    error
		wantClose bool
		wantText  string
	}{
		{
			name:      "not found closes and notifies",
			getErr:    &plans.NotFoundError{Scope: plans.ScopeShared, Name: "release"},
			wantClose: true,
			wantText:  "No shared plan",
		},
		{
			name:     "corrupt notifies without closing",
			getErr:   &plans.CorruptError{Scope: plans.ScopeShared, Name: "release", Err: errors.New("invalid frontmatter")},
			wantText: "corrupt",
		},
		{
			name:     "storage failure notifies without closing",
			getErr:   &plans.StorageError{Scope: plans.ScopeShared, Op: "get", Err: errors.New("device gone")},
			wantText: "storage failure",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m, svc, _, _ := newPlansTestModel(t)
			p := mustCreatePlan(t, svc, "release", "content")
			WithPlansService(&failingGetPlansService{
				Service: svc,
				failRef: plans.SharedRef("release"),
				err:     tt.getErr,
			})(m)

			sizeDialogs(t, m)
			openDialog(t, m, dialog.NewPlanDetailDialog(p))
			openDialog(t, m, dialog.NewHelpDialog(nil))

			// Repeated refreshes neither notify nor try to close the buried
			// detail.
			for range 2 {
				_, cmd := m.Update(messages.RefreshPlansMsg{})
				msgs := collectMsgs(cmd)
				assert.Empty(t, notificationTexts(msgs), "a buried failing detail must not notify on refresh")
				_, closed := firstOfType[dialog.CloseDialogMsg](msgs)
				assert.False(t, closed, "a buried detail must never pop the covering dialog")
			}

			// The covering dialog closes, the failing detail surfaces: the
			// next refresh notifies exactly once, closing only a vanished
			// plan.
			updated, _ := m.dialogMgr.Update(dialog.CloseDialogMsg{})
			m.dialogMgr = updated.(dialog.Manager)

			_, cmd := m.Update(messages.RefreshPlansMsg{})
			msgs := collectMsgs(cmd)
			_, closed := firstOfType[dialog.CloseDialogMsg](msgs)
			assert.Equal(t, tt.wantClose, closed, "only a vanished plan closes the surfaced detail")

			texts := notificationTexts(msgs)
			require.Len(t, texts, 1, "the surfaced failing detail notifies exactly once")
			assert.Contains(t, texts[0], tt.wantText)
		})
	}
}
