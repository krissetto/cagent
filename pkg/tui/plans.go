package tui

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/plans"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools/builtin/plan"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

// defaultPlanMutationTimeout bounds every plan persistence call issued from
// the TUI (create, update, set-status, delete). Those writes take a
// cross-process file lock, so a lock wedged by another process must surface
// as an actionable timeout notification instead of freezing the event loop
// indefinitely. Reads never take that lock and stay unbounded.
const defaultPlanMutationTimeout = 5 * time.Second

// WithPlansService injects the plans host service, replacing the lazily
// built default over plan.SharedStorage(). Intended for tests, which run
// against temp-backed storage instead of the user's data directory.
func WithPlansService(svc plans.Service) Option {
	return func(m *appModel) {
		if svc != nil {
			m.plansSvc = svc
		}
	}
}

// planMutationTimeoutOrDefault returns the persistence timeout for this
// model, falling back to the package default. Tests set the field to keep
// timeout scenarios fast.
func (m *appModel) planMutationTimeoutOrDefault() time.Duration {
	return cmp.Or(m.planMutationTimeout, defaultPlanMutationTimeout)
}

// plansService returns the host-facing plan service, building it on first
// use. The lazy build matters: plan.SharedStorage() memoizes the plans
// directory process-wide, so it must not be touched before the CLI pre-run
// has applied path configuration (e.g. --data-dir).
func (m *appModel) plansService() plans.Service {
	if m.plansSvc == nil {
		m.plansSvc = plans.NewService(plan.SharedStorage())
	}
	return m.plansSvc
}

// currentPlanSessionID identifies the active session whose plan is included
// in listings. Only this session is ever consulted; plan files left behind
// by other sessions are never enumerated.
func (m *appModel) currentPlanSessionID() string {
	if m.application == nil {
		return ""
	}
	if sess := m.application.Session(); sess != nil {
		return sess.ID
	}
	return ""
}

func (m *appModel) loadPlanList() (plans.ListResult, error) {
	return m.plansService().List(m.ctx(), plans.ListOptions{SessionID: m.currentPlanSessionID()})
}

// planDialogOpen reports whether any dialog of the /plans flow is on the
// stack — topmost or buried under another dialog — i.e. plan data is on
// screen and worth live-refreshing.
func (m *appModel) planDialogOpen() bool {
	return m.dialogMgr.HasDialog(func(d dialog.Dialog) bool {
		_, ok := d.(dialog.PlanDialog)
		return ok
	})
}

func (m *appModel) handleShowPlanBrowser() (tea.Model, tea.Cmd) {
	result, err := m.loadPlanList()
	if err != nil {
		return m, planErrorCmd(err)
	}
	cmds := []tea.Cmd{core.CmdHandler(dialog.OpenDialogMsg{Model: dialog.NewPlanBrowserDialog(result)})}
	cmds = append(cmds, planWarningsCmds(result.Warnings)...)
	return m, tea.Sequence(cmds...)
}

func (m *appModel) handleRefreshPlans() (tea.Model, tea.Cmd) {
	return m, tea.Sequence(m.planRefreshCmds(true)...)
}

// planRefreshCmds reloads plan data into the open plan dialogs: fresh rows
// for the browser and fresh content for an open detail dialog. The data
// messages are broadcast so a browser buried under the detail updates too.
func (m *appModel) planRefreshCmds(notifyWarnings bool) []tea.Cmd {
	cmds := m.planListRefreshCmds(notifyWarnings)
	cmds = append(cmds, m.planDetailRefreshCmds()...)
	return cmds
}

func (m *appModel) planListRefreshCmds(notifyWarnings bool) []tea.Cmd {
	result, err := m.loadPlanList()
	if err != nil {
		return []tea.Cmd{planErrorCmd(err)}
	}
	cmds := []tea.Cmd{core.CmdHandler(dialog.PlanBrowserDataMsg{Result: result})}
	if notifyWarnings {
		cmds = append(cmds, planWarningsCmds(result.Warnings)...)
	}
	return cmds
}

// openPlanDetailRefs returns the refs shown by every open plan detail
// dialog, including ones buried under other dialogs. The predicate never
// matches: it is used as a visitor over the whole stack.
func (m *appModel) openPlanDetailRefs() []plans.Ref {
	var refs []plans.Ref
	m.dialogMgr.HasDialog(func(d dialog.Dialog) bool {
		if viewer, ok := d.(dialog.PlanDetailViewer); ok {
			refs = append(refs, viewer.PlanRef())
		}
		return false
	})
	return refs
}

// planDetailRefreshCmds re-fetches the plan shown by every open detail
// dialog, buried ones included; the data messages are broadcast and each
// detail applies only its own plan. Read failures surface only for the
// detail that is the topmost dialog: a plan that disappeared closes it,
// with one notification, and any other failure notifies without closing. A
// buried detail cannot be closed without popping the wrong dialog, and
// notifying on every refresh would repeat the same warning indefinitely, so
// a buried failing ref is skipped silently: it is notified (and closed if
// gone) once it surfaces and the next refresh runs.
func (m *appModel) planDetailRefreshCmds() []tea.Cmd {
	var cmds []tea.Cmd
	for _, ref := range m.openPlanDetailRefs() {
		p, err := m.plansService().Get(m.ctx(), ref)
		if err != nil {
			if viewer, ok := m.dialogMgr.TopDialog().(dialog.PlanDetailViewer); !ok || viewer.PlanRef() != ref {
				continue
			}
			var notFound *plans.NotFoundError
			if errors.As(err, &notFound) {
				cmds = append(cmds, core.CmdHandler(dialog.CloseDialogMsg{}))
			}
			cmds = append(cmds, planErrorCmd(err))
			continue
		}
		cmds = append(cmds, core.CmdHandler(dialog.PlanDetailDataMsg{Plan: p}))
	}
	return cmds
}

func (m *appModel) handleOpenPlanDetail(ref plans.Ref) (tea.Model, tea.Cmd) {
	p, err := m.plansService().Get(m.ctx(), ref)
	if err != nil {
		return m, planErrorCmd(err)
	}
	return m, core.CmdHandler(dialog.OpenDialogMsg{Model: dialog.NewPlanDetailDialog(p)})
}

func (m *appModel) handleExportPlan(ref plans.Ref) (tea.Model, tea.Cmd) {
	path := filepath.Join(m.activeWorkingDir(), planExportFilename(ref))
	// Refuse to overwrite: the default path is deterministic, so a repeat
	// export must fail loudly instead of clobbering the previous file.
	if _, err := os.Stat(path); err == nil {
		return m, notification.ErrorCmd(
			path + " already exists — move it away, or export to a custom path with 'docker agent plans export'.")
	} else if !errors.Is(err, fs.ErrNotExist) {
		return m, notification.ErrorCmd(fmt.Sprintf("Cannot export to %s: %v", path, err))
	}

	result, err := m.plansService().Export(m.ctx(), plans.ExportRequest{Ref: ref, Path: path})
	if err != nil {
		return m, planErrorCmd(err)
	}
	return m, notification.SuccessCmd(fmt.Sprintf("Exported %s plan to %s (%d bytes)", result.Scope, result.Path, result.BytesWritten))
}

// planExportFilename is the deterministic default export target: the plan
// name for shared plans, a short session marker for session plans.
func planExportFilename(ref plans.Ref) string {
	if ref.Scope == plans.ScopeSession {
		return "session-plan-" + planShortSessionID(ref.SessionID) + ".md"
	}
	return ref.Name + ".md"
}

func planShortSessionID(id string) string {
	if r := []rune(id); len(r) > 8 {
		return string(r[:8])
	}
	return id
}

// activeWorkingDir is the working directory of the active tab's runtime,
// falling back to the process working directory.
func (m *appModel) activeWorkingDir() string {
	if m.supervisor != nil {
		if runner := m.supervisor.GetRunner(m.supervisor.ActiveID()); runner != nil && runner.WorkingDir != "" {
			return runner.WorkingDir
		}
	}
	if wd, err := os.Getwd(); err == nil && wd != "" {
		return wd
	}
	return "."
}

// planStatusResultMsg reports a completed asynchronous set-status write.
type planStatusResultMsg struct {
	plan plans.Plan
	err  error
}

// planDeleteResultMsg reports a completed asynchronous delete.
type planDeleteResultMsg struct {
	ref             plans.Ref
	expectedVersion int
	err             error
}

// planWriteResultMsg reports a completed asynchronous editor-driven create
// or update. draftPath is the draft file the content came from; it is only
// removed after the service confirmed the write (or the draft turned out
// empty) and is preserved on every error so no edit is ever lost.
type planWriteResultMsg struct {
	ref       plans.Ref
	create    bool
	draftPath string
	plan      plans.Plan
	// emptyDraft marks a draft with no content: nothing was written and the
	// draft file was removed.
	emptyDraft bool
	// readErr reports a draft that could not be read (or was refused as
	// non-regular or oversized); the write was never attempted.
	readErr error
	// err reports a failed persistence call for a successfully read draft.
	err error
}

// handleSetPlanStatus starts the guarded status write in a command so a
// contended plans lock can never freeze the event loop; the outcome arrives
// as a planStatusResultMsg.
func (m *appModel) handleSetPlanStatus(msg messages.SetPlanStatusMsg) (tea.Model, tea.Cmd) {
	svc := m.plansService()
	ctx, timeout := m.ctx(), m.planMutationTimeoutOrDefault()
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		expected := msg.ExpectedVersion
		p, err := svc.SetStatus(ctx, plans.SetStatusRequest{
			Ref:             msg.Ref,
			Status:          msg.Status,
			ExpectedVersion: &expected,
		})
		return planStatusResultMsg{plan: p, err: err}
	}
}

func (m *appModel) handlePlanStatusResult(msg planStatusResultMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		cmd := m.planWriteFailureCmd(msg.err)
		return m, cmd
	}
	cmds := []tea.Cmd{notification.SuccessCmd(fmt.Sprintf("Status of %q set to %q (now v%d)", msg.plan.Name, msg.plan.Status, planVersionOf(msg.plan)))}
	cmds = append(cmds, m.planRefreshCmds(false)...)
	return m, tea.Sequence(cmds...)
}

// handleDeletePlan starts the guarded delete in a command; the outcome
// arrives as a planDeleteResultMsg.
func (m *appModel) handleDeletePlan(msg messages.DeletePlanMsg) (tea.Model, tea.Cmd) {
	svc := m.plansService()
	ctx, timeout := m.ctx(), m.planMutationTimeoutOrDefault()
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		expected := msg.ExpectedVersion
		err := svc.Delete(ctx, plans.DeleteRequest{Ref: msg.Ref, ExpectedVersion: &expected})
		return planDeleteResultMsg{ref: msg.Ref, expectedVersion: msg.ExpectedVersion, err: err}
	}
}

func (m *appModel) handlePlanDeleteResult(msg planDeleteResultMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		cmd := m.planWriteFailureCmd(msg.err)
		return m, cmd
	}
	cmds := []tea.Cmd{notification.SuccessCmd(fmt.Sprintf("Deleted shared plan %q (was v%d)", msg.ref.Name, msg.expectedVersion))}
	// A detail dialog showing the deleted plan has nothing left to show.
	// The browser row is only removed by the refresh below — never before
	// the service confirmed the delete.
	if viewer, ok := m.dialogMgr.TopDialog().(dialog.PlanDetailViewer); ok && viewer.PlanRef() == msg.ref {
		cmds = append(cmds, core.CmdHandler(dialog.CloseDialogMsg{}))
	}
	cmds = append(cmds, m.planListRefreshCmds(false)...)
	return m, tea.Sequence(cmds...)
}

func (m *appModel) handleCreatePlan(name string) (tea.Model, tea.Cmd) {
	name = strings.TrimSpace(name)
	// Validate before opening the editor so a bad name fails fast instead
	// of discarding a drafted document.
	if err := plan.ValidateName(name); err != nil {
		return m, notification.ErrorCmd(err.Error())
	}
	tmpPath, err := planDraftFile("cagent-plan-new-*.md", "")
	if err != nil {
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to create draft file: %v", err))
	}
	cmd := m.execPlanEditor(planEditorClosedMsg{ref: plans.SharedRef(name), create: true, path: tmpPath})
	return m, cmd
}

func (m *appModel) handleEditPlan(msg messages.EditPlanMsg) (tea.Model, tea.Cmd) {
	p, err := m.plansService().Get(m.ctx(), msg.Ref)
	if err != nil {
		return m, planErrorCmd(err)
	}
	// The plan moved on since the version on screen: refresh instead of
	// editing a base the user has not seen.
	if planVersionOf(p) != msg.ExpectedVersion {
		cmds := []tea.Cmd{notification.WarningCmd(fmt.Sprintf(
			"Plan %q is at v%d now (you read v%d). Data refreshed — review and press e again.",
			p.Name, planVersionOf(p), msg.ExpectedVersion))}
		cmds = append(cmds, m.planRefreshCmds(false)...)
		return m, tea.Sequence(cmds...)
	}
	tmpPath, err := planDraftFile("cagent-plan-"+msg.Ref.Name+"-*.md", p.Content)
	if err != nil {
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to create draft file: %v", err))
	}
	cmd := m.execPlanEditor(planEditorClosedMsg{ref: msg.Ref, expectedVersion: msg.ExpectedVersion, path: tmpPath})
	return m, cmd
}

// planEditorClosedMsg reports that the external editor for a plan draft has
// exited; the app model then reads the draft and performs the guarded write.
type planEditorClosedMsg struct {
	ref             plans.Ref
	expectedVersion int
	create          bool
	path            string
	err             error
}

// planDraftFile writes content to a fresh temp markdown file and returns its
// path.
func planDraftFile(pattern, content string) (string, error) {
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", err
	}
	path := f.Name()
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

// execPlanEditor opens result.path in $VISUAL/$EDITOR via tea.ExecProcess
// and reports back with result once the editor exits.
func (m *appModel) execPlanEditor(result planEditorClosedMsg) tea.Cmd {
	parts := strings.Fields(cmp.Or(os.Getenv("VISUAL"), os.Getenv("EDITOR")))
	if len(parts) == 0 {
		if goruntime.GOOS == "windows" {
			parts = []string{"notepad"}
		} else {
			parts = []string{"vi"}
		}
	}
	args := append(parts[1:], result.path)
	// The editor process is owned by tea.ExecProcess, so exec.Command is intentional.
	cmd := exec.Command(parts[0], args...) //nolint:noctx // owned by tea.ExecProcess
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		result.err = err
		return result
	})
}

func (m *appModel) handlePlanEditorClosed(msg planEditorClosedMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		_ = os.Remove(msg.path)
		return m, notification.ErrorCmd(fmt.Sprintf("Editor error: %v", msg.err))
	}

	// Both the draft read and the guarded write run in a command: reading in
	// Update would stall the event loop on a draft path swapped for a FIFO
	// or device and allocate unbounded memory for a runaway file, and a
	// contended plans lock could freeze it just the same. The draft file
	// outlives the command until the service confirms the write.
	svc := m.plansService()
	ctx, timeout := m.ctx(), m.planMutationTimeoutOrDefault()
	return m, func() tea.Msg {
		result := planWriteResultMsg{ref: msg.ref, create: msg.create, draftPath: msg.path}
		content, err := readPlanDraft(msg.path)
		if err != nil {
			result.readErr = err
			return result
		}
		if strings.TrimSpace(content) == "" {
			_ = os.Remove(msg.path)
			result.emptyDraft = true
			return result
		}
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		if msg.create {
			result.plan, result.err = svc.Create(ctx, plans.CreateRequest{Ref: msg.ref, Content: content})
		} else {
			expected := msg.expectedVersion
			result.plan, result.err = svc.Update(ctx, plans.UpdateRequest{Ref: msg.ref, Content: content, ExpectedVersion: &expected})
		}
		return result
	}
}

// readPlanDraft reads the edited draft back, bounded and hang-safe: the open
// cannot block on a FIFO with no writer (plan.OpenContentFile), only a
// regular file is accepted — checked on the opened descriptor, so a
// concurrent path swap cannot slip a device past the check — and the read is
// capped at the plan content limit so a runaway draft can never exhaust
// memory. The service would refuse over-cap content anyway; refusing here
// skips reading and shipping megabytes that cannot be persisted.
func readPlanDraft(path string) (string, error) {
	f, err := plan.OpenContentFile(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("draft %s is not a regular file", path)
	}
	if info.Size() > plan.MaxPlanContentSize {
		return "", fmt.Errorf("draft exceeds the maximum plan size (%d bytes; max %d)", info.Size(), plan.MaxPlanContentSize)
	}

	// Read one byte past the cap so a draft that grew since the size check
	// is still detected without trusting the stat size.
	data, err := io.ReadAll(io.LimitReader(f, plan.MaxPlanContentSize+1))
	if err != nil {
		return "", err
	}
	if len(data) > plan.MaxPlanContentSize {
		return "", fmt.Errorf("draft exceeds the maximum plan size (max %d bytes)", plan.MaxPlanContentSize)
	}
	return string(data), nil
}

func (m *appModel) handlePlanWriteResult(msg planWriteResultMsg) (tea.Model, tea.Cmd) {
	switch {
	case msg.readErr != nil:
		return m, tea.Sequence(
			notification.ErrorCmd(fmt.Sprintf("Failed to read edited plan: %v", msg.readErr)),
			notification.InfoCmd("Your draft is kept at "+msg.draftPath))
	case msg.emptyDraft:
		if msg.create {
			return m, notification.InfoCmd(fmt.Sprintf("Plan %q not created: the draft was empty.", msg.ref.Name))
		}
		return m, notification.InfoCmd(fmt.Sprintf("Plan %q left unchanged: an empty draft is never committed.", msg.ref.Name))
	case msg.err != nil:
		cmd := m.planEditorFailureCmd(msg.err, msg.draftPath)
		return m, cmd
	}
	_ = os.Remove(msg.draftPath)

	verb := "Updated"
	if msg.create {
		verb = "Created"
	}
	cmds := []tea.Cmd{notification.SuccessCmd(fmt.Sprintf("%s shared plan %q (now v%d)", verb, msg.plan.Name, planVersionOf(msg.plan)))}
	cmds = append(cmds, m.planRefreshCmds(false)...)
	return m, tea.Sequence(cmds...)
}

// planEditorFailureCmd reports a failed editor-driven write. The draft file
// is deliberately kept so a conflict, timeout, or storage failure never
// loses the user's edits; the newer plan content stays intact and is re-read
// into the open dialogs.
func (m *appModel) planEditorFailureCmd(err error, draftPath string) tea.Cmd {
	if timeout := m.planTimeoutCmd(err); timeout != nil {
		return tea.Sequence(timeout, notification.InfoCmd("Your draft is kept at "+draftPath))
	}
	var conflict *plans.ConflictError
	if errors.As(err, &conflict) {
		text := fmt.Sprintf(
			"Version conflict on %q: it is at v%d, you edited v%d. Your draft is kept at %s — refresh and retry from it.",
			conflict.Name, conflict.Current, conflict.Expected, draftPath)
		if conflict.Expected == 0 {
			text = fmt.Sprintf(
				"Plan %q already exists (v%d). Your draft is kept at %s — pick another name or edit the existing plan.",
				conflict.Name, conflict.Current, draftPath)
		}
		cmds := []tea.Cmd{notification.ErrorCmd(text)}
		cmds = append(cmds, m.planRefreshCmds(false)...)
		return tea.Sequence(cmds...)
	}
	return tea.Sequence(planErrorCmd(err), notification.InfoCmd("Your draft is kept at "+draftPath))
}

// planWriteFailureCmd reports a failed status/delete write. Conflicts
// refresh the open dialogs so the newer version is visible right away.
func (m *appModel) planWriteFailureCmd(err error) tea.Cmd {
	if timeout := m.planTimeoutCmd(err); timeout != nil {
		return timeout
	}
	var conflict *plans.ConflictError
	if errors.As(err, &conflict) {
		cmds := []tea.Cmd{notification.ErrorCmd(fmt.Sprintf(
			"Version conflict on %q: it changed to v%d since you read v%d. Data refreshed — review and retry.",
			conflict.Name, conflict.Current, conflict.Expected))}
		cmds = append(cmds, m.planRefreshCmds(false)...)
		return tea.Sequence(cmds...)
	}
	return planErrorCmd(err)
}

// planTimeoutCmd returns the actionable notification for a persistence
// operation that hit the bounded mutation timeout — most likely the
// cross-process plans lock is held by a wedged process — or nil when err is
// not a timeout.
func (m *appModel) planTimeoutCmd(err error) tea.Cmd {
	if !errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	return notification.ErrorCmd(fmt.Sprintf(
		"Plan write timed out after %s — the plan store may be locked by another process. Retry shortly.",
		m.planMutationTimeoutOrDefault()))
}

// planVersionOf reads a shared plan's version defensively; the service
// always sets it for shared plans.
func planVersionOf(p plans.Plan) int {
	if p.Version == nil {
		return 0
	}
	return *p.Version
}

// planErrorCmd maps the typed pkg/plans errors onto distinguishable
// user-facing notifications, never classifying by error text.
func planErrorCmd(err error) tea.Cmd {
	var (
		conflict    *plans.ConflictError
		notFound    *plans.NotFoundError
		validation  *plans.ValidationError
		corrupt     *plans.CorruptError
		unsupported *plans.UnsupportedError
	)
	switch {
	case errors.As(err, &conflict):
		return notification.ErrorCmd(fmt.Sprintf(
			"Version conflict on plan %q: expected v%d but it is at v%d. Refresh (r) and retry.",
			conflict.Name, conflict.Expected, conflict.Current))
	case errors.As(err, &notFound):
		return notification.WarningCmd(fmt.Sprintf("No %s plan %q — it may have been deleted; refresh (r).", notFound.Scope, notFound.Name))
	case errors.As(err, &validation):
		return notification.ErrorCmd("Invalid input: " + validation.Message)
	case errors.As(err, &corrupt):
		return notification.ErrorCmd(fmt.Sprintf("Plan %q is corrupt and cannot be read (%v). Delete it to recover.", corrupt.Name, corrupt.Err))
	case errors.As(err, &unsupported):
		return notification.InfoCmd("Unsupported: " + unsupported.Error())
	default:
		return notification.ErrorCmd("Plan storage failure: " + err.Error())
	}
}

func planWarningsCmds(warnings []string) []tea.Cmd {
	if len(warnings) == 0 {
		return nil
	}
	return []tea.Cmd{notification.WarningCmd(fmt.Sprintf(
		"%d plan(s) could not be read: %s", len(warnings), strings.Join(warnings, "; ")))}
}

// handleSessionPlanUpdatedEvent forwards the event to the chat page like any
// runtime event and live-refreshes open plan dialogs when the active
// session's plan changed.
func (m *appModel) handleSessionPlanUpdatedEvent(msg *runtime.SessionPlanUpdatedEvent) (tea.Model, tea.Cmd) {
	if name := msg.GetAgentName(); name != "" {
		m.sessionState.SetCurrentAgentName(name)
	}
	chatCmd := m.updateChatCmd(msg)
	var refresh tea.Cmd
	if m.planDialogOpen() && msg.SessionID == m.currentPlanSessionID() {
		refresh = tea.Sequence(m.planRefreshCmds(false)...)
	}
	return m, tea.Batch(chatCmd, refresh)
}

// handlePlanChangedEvent live-refreshes open plan dialogs after an agent
// mutated a shared plan. Shared plans are scope-global, so the refresh does
// not depend on which session emitted the event.
func (m *appModel) handlePlanChangedEvent(msg *runtime.PlanChangedEvent) (tea.Model, tea.Cmd) {
	if name := msg.GetAgentName(); name != "" {
		m.sessionState.SetCurrentAgentName(name)
	}
	chatCmd := m.updateChatCmd(msg)
	var refresh tea.Cmd
	if m.planDialogOpen() {
		refresh = tea.Sequence(m.planRefreshCmds(false)...)
	}
	return m, tea.Batch(chatCmd, refresh)
}
