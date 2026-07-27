package tui

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/plans"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools/builtin/plan"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

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

// planDialogOpen reports whether the topmost dialog belongs to the /plans
// flow, i.e. plan data is on screen and worth live-refreshing.
func (m *appModel) planDialogOpen() bool {
	_, ok := m.dialogMgr.TopDialog().(dialog.PlanDialog)
	return ok
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

// planDetailRefreshCmds re-fetches the plan shown by an open detail dialog.
// A plan that disappeared closes the detail instead of leaving stale content
// on screen.
func (m *appModel) planDetailRefreshCmds() []tea.Cmd {
	viewer, ok := m.dialogMgr.TopDialog().(dialog.PlanDetailViewer)
	if !ok {
		return nil
	}
	p, err := m.plansService().Get(m.ctx(), viewer.PlanRef())
	if err != nil {
		var notFound *plans.NotFoundError
		if errors.As(err, &notFound) {
			return []tea.Cmd{core.CmdHandler(dialog.CloseDialogMsg{}), planErrorCmd(err)}
		}
		return []tea.Cmd{planErrorCmd(err)}
	}
	return []tea.Cmd{core.CmdHandler(dialog.PlanDetailDataMsg{Plan: p})}
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

func (m *appModel) handleSetPlanStatus(msg messages.SetPlanStatusMsg) (tea.Model, tea.Cmd) {
	expected := msg.ExpectedVersion
	p, err := m.plansService().SetStatus(m.ctx(), plans.SetStatusRequest{
		Ref:             msg.Ref,
		Status:          msg.Status,
		ExpectedVersion: &expected,
	})
	if err != nil {
		cmd := m.planWriteFailureCmd(err)
		return m, cmd
	}
	cmds := []tea.Cmd{notification.SuccessCmd(fmt.Sprintf("Status of %q set to %q (now v%d)", p.Name, p.Status, planVersionOf(p)))}
	cmds = append(cmds, m.planRefreshCmds(false)...)
	return m, tea.Sequence(cmds...)
}

func (m *appModel) handleDeletePlan(msg messages.DeletePlanMsg) (tea.Model, tea.Cmd) {
	expected := msg.ExpectedVersion
	err := m.plansService().Delete(m.ctx(), plans.DeleteRequest{Ref: msg.Ref, ExpectedVersion: &expected})
	if err != nil {
		cmd := m.planWriteFailureCmd(err)
		return m, cmd
	}
	cmds := []tea.Cmd{notification.SuccessCmd(fmt.Sprintf("Deleted shared plan %q (was v%d)", msg.Ref.Name, msg.ExpectedVersion))}
	// A detail dialog showing the deleted plan has nothing left to show.
	// The browser row is only removed by the refresh below — never before
	// the service confirmed the delete.
	if viewer, ok := m.dialogMgr.TopDialog().(dialog.PlanDetailViewer); ok && viewer.PlanRef() == msg.Ref {
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
	data, err := os.ReadFile(msg.path)
	if err != nil {
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to read edited plan: %v", err))
	}
	content := string(data)
	if strings.TrimSpace(content) == "" {
		_ = os.Remove(msg.path)
		if msg.create {
			return m, notification.InfoCmd(fmt.Sprintf("Plan %q not created: the draft was empty.", msg.ref.Name))
		}
		return m, notification.InfoCmd(fmt.Sprintf("Plan %q left unchanged: an empty draft is never committed.", msg.ref.Name))
	}

	var p plans.Plan
	if msg.create {
		p, err = m.plansService().Create(m.ctx(), plans.CreateRequest{Ref: msg.ref, Content: content})
	} else {
		expected := msg.expectedVersion
		p, err = m.plansService().Update(m.ctx(), plans.UpdateRequest{Ref: msg.ref, Content: content, ExpectedVersion: &expected})
	}
	if err != nil {
		cmd := m.planEditorFailureCmd(err, msg.path)
		return m, cmd
	}
	_ = os.Remove(msg.path)

	verb := "Updated"
	if msg.create {
		verb = "Created"
	}
	cmds := []tea.Cmd{notification.SuccessCmd(fmt.Sprintf("%s shared plan %q (now v%d)", verb, p.Name, planVersionOf(p)))}
	cmds = append(cmds, m.planRefreshCmds(false)...)
	return m, tea.Sequence(cmds...)
}

// planEditorFailureCmd reports a failed editor-driven write. The draft file
// is deliberately kept so a conflict or storage failure never loses the
// user's edits; the newer plan content stays intact and is re-read into the
// open dialogs.
func (m *appModel) planEditorFailureCmd(err error, draftPath string) tea.Cmd {
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
