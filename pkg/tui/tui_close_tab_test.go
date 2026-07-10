package tui

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/sessiontitle"
	"github.com/docker/docker-agent/pkg/tools"
	skillstool "github.com/docker/docker-agent/pkg/tools/builtin/skills"
	mcptools "github.com/docker/docker-agent/pkg/tools/mcp"
	"github.com/docker/docker-agent/pkg/tui/components/editor"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/service/supervisor"
)

type closeTabRuntime struct {
	runningSubagents atomic.Bool
}

func newCloseTabRuntime(_ string, runningSubagents bool) *closeTabRuntime {
	r := &closeTabRuntime{}
	r.runningSubagents.Store(runningSubagents)
	return r
}

func (*closeTabRuntime) CurrentAgentInfo(context.Context) runtime.CurrentAgentInfo {
	return runtime.CurrentAgentInfo{}
}
func (*closeTabRuntime) CurrentAgentName(context.Context) string                 { return "root" }
func (*closeTabRuntime) SetCurrentAgent(context.Context, string) error           { return nil }
func (*closeTabRuntime) CurrentAgentTools(context.Context) ([]tools.Tool, error) { return nil, nil }
func (*closeTabRuntime) CurrentAgentToolsetStatuses() []tools.ToolsetStatus      { return nil }
func (*closeTabRuntime) RestartToolset(context.Context, string) error            { return nil }
func (*closeTabRuntime) EmitStartupInfo(context.Context, *session.Session, runtime.EventSink) {
}
func (*closeTabRuntime) EmitAgentInfo(context.Context, runtime.EventSink) {}
func (*closeTabRuntime) ResetStartupInfo()                                {}
func (*closeTabRuntime) RunStream(context.Context, *session.Session) <-chan runtime.Event {
	ch := make(chan runtime.Event)
	close(ch)
	return ch
}

func (*closeTabRuntime) Run(context.Context, *session.Session) ([]session.Message, error) {
	return nil, nil
}
func (*closeTabRuntime) Resume(context.Context, runtime.ResumeRequest) {}
func (*closeTabRuntime) ResumeElicitation(context.Context, tools.ElicitationAction, map[string]any) error {
	return nil
}
func (*closeTabRuntime) SessionStore() session.Store { return nil }
func (*closeTabRuntime) Summarize(context.Context, *session.Session, string, runtime.EventSink) {
}
func (*closeTabRuntime) PermissionsInfo() *runtime.PermissionsInfo { return nil }
func (*closeTabRuntime) CurrentAgentSkillsToolset() *skillstool.ToolSet {
	return nil
}

func (*closeTabRuntime) RunSkillFork(context.Context, *session.Session, skillstool.RunSkillArgs, runtime.EventSink) (*tools.ToolCallResult, error) {
	return nil, nil
}

func (*closeTabRuntime) CurrentMCPPrompts(context.Context) map[string]mcptools.PromptInfo {
	return nil
}

func (*closeTabRuntime) ExecuteMCPPrompt(context.Context, string, map[string]string) (string, error) {
	return "", nil
}

func (*closeTabRuntime) UpdateSessionTitle(context.Context, *session.Session, string) error {
	return nil
}
func (*closeTabRuntime) TitleGenerator(context.Context) *sessiontitle.Generator { return nil }
func (*closeTabRuntime) Steer(context.Context, runtime.QueuedMessage) error     { return nil }
func (*closeTabRuntime) FollowUp(context.Context, runtime.QueuedMessage) error  { return nil }
func (*closeTabRuntime) SetAgentModel(context.Context, string, string) error    { return nil }
func (*closeTabRuntime) CycleAgentThinkingLevel(context.Context, string) (effort.Level, error) {
	return "", runtime.ErrUnsupported
}

func (*closeTabRuntime) SetAgentThinkingLevel(context.Context, string, effort.Level) (effort.Level, error) {
	return "", runtime.ErrUnsupported
}
func (*closeTabRuntime) AvailableModels(context.Context) []runtime.ModelChoice { return nil }
func (*closeTabRuntime) SupportsModelSwitching() bool                          { return false }
func (*closeTabRuntime) OnToolsChanged(func(runtime.Event))                    {}
func (*closeTabRuntime) OnBackgroundEvent(func(runtime.Event))                 {}
func (*closeTabRuntime) QueueStatus() runtime.QueueStatus                      { return runtime.QueueStatus{} }
func (*closeTabRuntime) TogglePause(context.Context) (bool, error)             { return false, nil }
func (*closeTabRuntime) Close() error                                          { return nil }
func (r *closeTabRuntime) HasRunningSubagents(string) bool                     { return r.runningSubagents.Load() }

var _ runtime.Runtime = (*closeTabRuntime)(nil)

func addCloseTabTestSession(t *testing.T, m *appModel, id string, rt runtime.Runtime, cleanup func(), opts ...app.Opt) {
	t.Helper()
	sess := session.New(session.WithID(id))
	a := app.New(t.Context(), rt, sess, opts...)
	m.supervisor.AddSession(t.Context(), a, sess, t.TempDir(), cleanup)
	m.chatPages[id] = &mockChatPage{}
	m.editors[id] = &mockEditor{}
	m.sessionStates[id] = service.NewSessionState(sess)
}

func newCloseTabTestModel(t *testing.T) *appModel {
	t.Helper()
	m, _ := newTestModel(t)
	m.supervisor = supervisor.New(nil)
	m.chatPages = map[string]chat.Page{}
	m.editors = map[string]editor.Editor{}
	m.sessionStates = map[string]*service.SessionState{}
	m.pendingRestores = map[string]string{}
	m.pendingSidebarCollapsed = map[string]bool{}
	m.stashedDialogs = map[string]stashedDialog{}
	return m
}

func TestCloseRootWithRunningSubagentsRequiresConfirmation(t *testing.T) {
	t.Parallel()

	m := newCloseTabTestModel(t)
	addCloseTabTestSession(t, m, "root", newCloseTabRuntime("root", true), nil)
	addCloseTabTestSession(t, m, "other", newCloseTabRuntime("other", false), nil)

	_, cmd := m.handleCloseTab("root")
	require.NotNil(t, cmd)
	open, ok := cmd().(dialog.OpenDialogMsg)
	require.True(t, ok)
	_, _ = open.Model.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	view := open.Model.View()
	assert.Contains(t, view, "running subagents")
	assert.Contains(t, view, "interrupt")
	assert.Contains(t, view, "current work")
	assert.Contains(t, view, "close their tabs")
	assert.NotNil(t, m.supervisor.GetRunner("root"), "root stays open until confirmed")
	assert.NotNil(t, m.supervisor.GetRunner("other"))
}

func TestCloseRootWithoutRunningSubagentsClosesImmediately(t *testing.T) {
	t.Parallel()

	m := newCloseTabTestModel(t)
	shared := newCloseTabRuntime("shared", false)
	addCloseTabTestSession(t, m, "root", shared, nil)
	info := runtime.SubagentAttachInfo{NodeID: "node1", ParentSessionID: "root", ParentAgent: "root"}
	addCloseTabTestSession(t, m, "child", shared, nil, app.WithSubagentAttach(info))
	addCloseTabTestSession(t, m, "other", newCloseTabRuntime("other", false), nil)
	require.NotNil(t, m.supervisor.SwitchTo("other"))

	_, cmd := m.handleCloseTab("root")
	require.Nil(t, cmd)
	assert.Nil(t, m.supervisor.GetRunner("root"))
	assert.Nil(t, m.supervisor.GetRunner("child"), "attached idle subagent tab closes with its root")
	assert.NotNil(t, m.supervisor.GetRunner("other"))
}

func TestCloseRootWithRunningSubagentsConfirmedCascadesAttachedTabs(t *testing.T) {
	t.Parallel()

	m := newCloseTabTestModel(t)
	shared := newCloseTabRuntime("shared", true)
	other := newCloseTabRuntime("other", false)
	var cleanupCalls atomic.Int32

	addCloseTabTestSession(t, m, "root", shared, func() { cleanupCalls.Add(1) })
	info := runtime.SubagentAttachInfo{NodeID: "node1", ParentSessionID: "root", ParentAgent: "root"}
	addCloseTabTestSession(t, m, "child", shared, nil, app.WithSubagentAttach(info))
	addCloseTabTestSession(t, m, "other", other, nil)
	require.NotNil(t, m.supervisor.SwitchTo("other"))

	_, cmd := m.Update(dialog.CloseRootWithSubagentsConfirmedMsg{SessionID: "root"})
	require.Nil(t, cmd)

	assert.Nil(t, m.supervisor.GetRunner("root"))
	assert.Nil(t, m.supervisor.GetRunner("child"), "confirming root close closes attached subagent tabs")
	assert.NotNil(t, m.supervisor.GetRunner("other"))
	require.Eventually(t, func() bool { return cleanupCalls.Load() == 1 }, time.Second, 10*time.Millisecond)
}

func TestCloseAttachedSubagentTabSkipsRunningSubagentConfirmation(t *testing.T) {
	t.Parallel()

	m := newCloseTabTestModel(t)
	shared := newCloseTabRuntime("shared", true)
	info := runtime.SubagentAttachInfo{NodeID: "node1", ParentSessionID: "root", ParentAgent: "root"}
	addCloseTabTestSession(t, m, "child", shared, nil, app.WithSubagentAttach(info))
	addCloseTabTestSession(t, m, "other", newCloseTabRuntime("other", false), nil)
	require.NotNil(t, m.supervisor.SwitchTo("other"))

	_, cmd := m.handleCloseTab("child")
	require.Nil(t, cmd)
	assert.Nil(t, m.supervisor.GetRunner("child"))
	assert.NotNil(t, m.supervisor.GetRunner("other"))
}

func TestCloseRootWithSubagentsDialogYesConfirms(t *testing.T) {
	t.Parallel()

	d := dialog.NewCloseRootWithSubagentsDialog("root")
	_, cmd := d.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	require.NotNil(t, cmd)
	msgs := collectMsgs(cmd)
	assert.True(t, hasMsg[dialog.CloseDialogMsg](msgs))
	assert.True(t, hasMsg[dialog.CloseRootWithSubagentsConfirmedMsg](msgs))
}

func TestCloseRootWithSubagentsDialogCancelDoesNotConfirm(t *testing.T) {
	t.Parallel()

	d := dialog.NewCloseRootWithSubagentsDialog("root")
	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.NotNil(t, cmd)
	msgs := collectMsgs(cmd)
	assert.True(t, hasMsg[dialog.CloseDialogMsg](msgs))
	assert.False(t, hasMsg[dialog.CloseRootWithSubagentsConfirmedMsg](msgs))
}
