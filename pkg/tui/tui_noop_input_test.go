package tui

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
	pagechat "github.com/docker/docker-agent/pkg/tui/page/chat"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func populateScrollableRoot(t *testing.T) *appModel {
	t.Helper()
	root := wallClockRoot(t, 100, 35)
	for i := range 30 {
		msg := session.NewAgentMessage("root", &chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: fmt.Sprintf("message %02d\n%s", i, strings.Repeat("content line\n", 4)),
		})
		root.application.Session().Messages = append(root.application.Session().Messages, session.NewMessageItem(msg))
	}
	_ = root.chatPage.Init()
	root.handleWindowResize(100, 35)
	root.chatPage.ScrollToBottom()
	root.viewCacheValid = false
	return root
}

func TestEffectiveScrollInputRendersImmediately(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  tea.Msg
	}{
		{name: "wheel", msg: messages.PointerUpdateMsg{HasWheel: true, WheelDelta: -1, X: 1, Y: 1}},
		{name: "key", msg: tea.KeyPressMsg{Code: tea.KeyPgUp}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := populateScrollableRoot(t)
			root.focusedPanel = PanelContent
			_ = root.chatPage.FocusMessages()
			before := root.View().Content

			_, _ = root.Update(tc.msg)
			after := root.View().Content

			require.NotEqual(t, before, after, "effective input reused the stale root frame")
			root.ar.Stop()
		})
	}
}

func TestStreamChunkRendersBeforeRecoveryClick(t *testing.T) {
	for _, tc := range []struct {
		name string
		wrap func(tea.Msg) tea.Msg
	}{
		{name: "direct", wrap: func(msg tea.Msg) tea.Msg { return msg }},
		{name: "routed", wrap: func(msg tea.Msg) tea.Msg {
			return messages.RoutedMsg{SessionID: "profile", Inner: msg}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := wallClockRoot(t, 120, 40)
			seed := session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: "first chunk"})
			root.application.Session().Messages = append(root.application.Session().Messages, session.NewMessageItem(seed))
			_ = root.chatPage.Init()
			root.viewCacheValid = false
			_ = root.View()

			_, _ = root.Update(tc.wrap(runtime.AgentChoice("root", "profile", " STREAM-IDLE-MARKER")))
			immediate := root.View().Content
			_, _ = root.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 0, Y: root.height - 1})
			afterClick := root.View().Content

			require.Contains(t, ansi.Strip(immediate), "STREAM-IDLE-MARKER", "chunk stayed hidden until pointer input")
			require.Equal(t, immediate, afterClick, "inert boundary click repaired a stale frame")
			root.ar.Stop()
		})
	}
}

type cacheProgramAck struct{ done chan struct{} }

type cacheProgramModel struct{ root *appModel }

func (m *cacheProgramModel) Init() tea.Cmd { return nil }

func (m *cacheProgramModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if ack, ok := msg.(cacheProgramAck); ok {
		close(ack.done)
		return m, nil
	}
	updated, cmd := m.root.Update(msg)
	m.root = updated.(*appModel)
	return m, cmd
}

func (m *cacheProgramModel) View() tea.View { return m.root.View() }

type cacheProgramWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *cacheProgramWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *cacheProgramWriter) snapshot() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func TestActualProgramWritesAfterEffectiveIdleInput(t *testing.T) {
	root := populateScrollableRoot(t)
	model := &cacheProgramModel{root: root}
	writer := &cacheProgramWriter{}
	program := tea.NewProgram(model, tea.WithInput(nil), tea.WithOutput(writer), tea.WithWindowSize(120, 40))
	done := make(chan error, 1)
	go func() {
		_, err := program.Run()
		done <- err
	}()
	require.Eventually(t, func() bool { return writer.snapshot() != "" }, time.Second, time.Millisecond)
	ack := func() {
		done := make(chan struct{})
		program.Send(cacheProgramAck{done: done})
		<-done
	}
	ack()

	beforeWheel := len(writer.snapshot())
	program.Send(messages.PointerUpdateMsg{HasWheel: true, WheelDelta: -1, X: 1, Y: 1})
	ack()
	require.Eventually(t, func() bool { return len(writer.snapshot()) > beforeWheel }, time.Second, time.Millisecond,
		"effective wheel input produced no terminal write")

	program.Send(messages.PointerUpdateMsg{HasWheel: true, WheelDelta: 1_000_000, X: 1, Y: 1})
	ack()
	program.Send(runtime.AgentChoice("root", "profile", " PROGRAM-STREAM-MARKER"))
	ack()
	require.Eventually(t, func() bool {
		return strings.Contains(ansi.Strip(writer.snapshot()), "PROGRAM-STREAM-MARKER")
	}, time.Second, time.Millisecond, "stream chunk produced no terminal write while idle")

	program.Quit()
	require.NoError(t, <-done)
	root.ar.Stop()
}

func requireSameView(t *testing.T, want, got tea.View) {
	t.Helper()
	require.Equal(t, want.Content, got.Content)
	require.Equal(t, reflect.ValueOf(want.OnMouse).Pointer(), reflect.ValueOf(got.OnMouse).Pointer())
	require.Same(t, want.Cursor, got.Cursor)
	require.Equal(t, want.BackgroundColor, got.BackgroundColor)
	require.Equal(t, want.ForegroundColor, got.ForegroundColor)
	require.Equal(t, want.WindowTitle, got.WindowTitle)
	require.Same(t, want.ProgressBar, got.ProgressBar)
	require.Equal(t, want.AltScreen, got.AltScreen)
	require.Equal(t, want.ReportFocus, got.ReportFocus)
	require.Equal(t, want.DisableBracketedPasteMode, got.DisableBracketedPasteMode)
	require.Equal(t, want.MouseMode, got.MouseMode)
	require.Equal(t, want.KeyboardEnhancements, got.KeyboardEnhancements)
}

func TestSidebarWheelRestoresOnlyNoOpRootView(t *testing.T) {
	sess := &session.Session{ID: "sidebar-cache", Title: "sidebar-cache"}
	a := app.New(t.Context(), stubRuntime{}, sess)
	root := wallClockRoot(t, 160, 24)
	root.hideSidebar = false
	ss := service.NewSessionState(sess)
	ss.SetCurrentAgentName("agent-00")
	page := pagechat.New(root.ar, t.Context(), a, ss)
	page.SetRoutingID(sess.ID)
	root.application, root.chatPage, root.sessionState = a, page, ss
	root.chatPages[sess.ID], root.sessionStates[sess.ID] = page, ss
	root.handleWindowResize(160, 24)
	_ = root.Init()

	agents := make([]runtime.AgentDetails, 30)
	for i := range agents {
		agents[i] = runtime.AgentDetails{
			Name:        fmt.Sprintf("agent-%02d", i),
			Provider:    "provider",
			Model:       "model",
			Description: "sidebar content",
		}
	}
	_, _ = root.Update(runtime.TeamInfo(agents, "agent-00"))
	first := root.View()
	root.viewCache.OnMouse = func(tea.MouseMsg) tea.Cmd { return nil }
	root.viewCache.Cursor = tea.NewCursor(7, 8)
	cached := root.viewCache
	beforeSidebarVisual := sidebarVisualGeneration(root.chatPage)

	_, _ = root.Update(messages.PointerUpdateMsg{HasWheel: true, WheelDelta: -1, X: 150, Y: 1})
	require.True(t, root.viewCacheValid, "top-boundary sidebar wheel should restore the root cache")
	require.Equal(t, beforeSidebarVisual, sidebarVisualGeneration(root.chatPage))
	requireSameView(t, cached, root.viewCache)
	requireSameView(t, cached, root.View())

	_, _ = root.Update(messages.PointerUpdateMsg{HasWheel: true, WheelDelta: 1, X: 150, Y: 1})
	require.False(t, root.viewCacheValid, "effective sidebar wheel should invalidate the root cache")
	require.Greater(t, sidebarVisualGeneration(root.chatPage), beforeSidebarVisual)
	require.NotEqual(t, first.Content, root.View().Content)
	root.ar.Stop()
}

func TestDialogWheelRestoresOnlyNoOpRootView(t *testing.T) {
	root := wallClockRoot(t, 80, 20)
	bindings := make([]key.Binding, 80)
	for i := range bindings {
		bindings[i] = key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "action"))
	}
	_, _ = root.Update(dialog.OpenDialogMsg{Model: dialog.NewHelpDialog(bindings)})
	_ = root.View()
	root.viewCache.OnMouse = func(tea.MouseMsg) tea.Cmd { return nil }
	root.viewCache.Cursor = tea.NewCursor(7, 8)
	cached := root.viewCache

	_, _ = root.Update(messages.PointerUpdateMsg{HasWheel: true, WheelDelta: -1, X: 40, Y: 10})
	require.True(t, root.viewCacheValid, "top-boundary dialog wheel should restore the root cache")
	requireSameView(t, cached, root.viewCache)
	requireSameView(t, cached, root.View())

	_, _ = root.Update(messages.PointerUpdateMsg{HasWheel: true, WheelDelta: 1, X: 40, Y: 10})
	require.False(t, root.viewCacheValid, "effective dialog wheel should invalidate the root cache")
	require.NotEqual(t, cached.Content, root.View().Content)
	root.ar.Stop()
}

func TestDialogPointerMotionIsModalAndCacheAware(t *testing.T) {
	root := wallClockRoot(t, 120, 40)
	d := &pointerAuditDialog{row: 8, col: 30}
	_, _ = root.Update(dialog.OpenDialogMsg{Model: d})
	_ = root.View()
	root.viewCache.OnMouse = func(tea.MouseMsg) tea.Cmd { return nil }
	root.viewCache.Cursor = tea.NewCursor(7, 8)
	cached := root.viewCache

	beforeChat := root.chatPage.VisualGeneration()
	beforeSidebar := sidebarVisualGeneration(root.chatPage)
	beforeTab := tabVisualGeneration(root.tabBar)
	beforeDialog := dialogVisualGeneration(root.dialogMgr)
	beforeEditorLines := root.editorLines
	viewCalls := d.viewCalls

	// Exercise content, sidebar, tab, and resize-handle coordinates behind the
	// modal. None is interactive for the dialog, so each move is an exact no-op.
	for _, motion := range []tea.MouseMotionMsg{
		{X: 1, Y: 1},
		{X: root.width - 1, Y: 1},
		{X: 10, Y: root.contentHeight},
		{X: 10, Y: root.contentHeight + 1},
	} {
		_, _ = root.Update(messages.PointerUpdateMsg{Motion: &motion})
		require.True(t, root.viewCacheValid)
		requireSameView(t, cached, root.View())
	}
	require.Equal(t, viewCalls, d.viewCalls, "no-op motion rendered the dialog")
	require.Equal(t, beforeChat, root.chatPage.VisualGeneration())
	require.Equal(t, beforeSidebar, sidebarVisualGeneration(root.chatPage))
	require.Equal(t, beforeTab, tabVisualGeneration(root.tabBar))
	require.Equal(t, beforeDialog, dialogVisualGeneration(root.dialogMgr))
	require.Equal(t, beforeEditorLines, root.editorLines)
	require.False(t, root.isHoveringHandle)

	enter := tea.MouseMotionMsg{X: d.col + 2, Y: d.row + 5}
	_, _ = root.Update(messages.PointerUpdateMsg{Motion: &enter})
	require.False(t, root.viewCacheValid, "visible dialog hover did not invalidate")
	hovered := root.View()
	require.NotEqual(t, cached.Content, hovered.Content)
	require.Greater(t, dialogVisualGeneration(root.dialogMgr), beforeDialog)
	require.Equal(t, beforeChat, root.chatPage.VisualGeneration())
	require.Equal(t, beforeSidebar, sidebarVisualGeneration(root.chatPage))
	require.Equal(t, beforeTab, tabVisualGeneration(root.tabBar))

	leave := tea.MouseMotionMsg{X: 0, Y: 0}
	_, _ = root.Update(messages.PointerUpdateMsg{Motion: &leave})
	require.False(t, root.viewCacheValid, "leaving visible dialog hover did not invalidate")
	require.Equal(t, cached.Content, root.View().Content)
	root.ar.Stop()
}

func TestDialogDragCaptureOwnsMotionOutsideBounds(t *testing.T) {
	root := wallClockRoot(t, 120, 40)
	d := &pointerAuditDialog{row: 8, col: 30}
	_, _ = root.Update(dialog.OpenDialogMsg{Model: d})
	before := root.View()
	beforeChat := root.chatPage.VisualGeneration()
	beforeSidebar := sidebarVisualGeneration(root.chatPage)
	beforeTab := tabVisualGeneration(root.tabBar)
	beforeEditorLines := root.editorLines

	_, _ = root.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: d.col + 2, Y: d.row + 1})
	outside := tea.MouseMotionMsg{Button: tea.MouseLeft, X: 1, Y: root.contentHeight}
	_, _ = root.Update(messages.PointerUpdateMsg{Motion: &outside})
	require.False(t, root.viewCacheValid)
	require.NotEqual(t, before.Content, root.View().Content, "captured drag did not move dialog outside its bounds")
	require.Equal(t, beforeChat, root.chatPage.VisualGeneration())
	require.Equal(t, beforeSidebar, sidebarVisualGeneration(root.chatPage))
	require.Equal(t, beforeTab, tabVisualGeneration(root.tabBar))
	require.Equal(t, beforeEditorLines, root.editorLines)
	require.False(t, root.isDragging, "covered root resize handle started dragging")
	root.ar.Stop()
}

type pointerAuditDialog struct {
	dialog.BaseDialog

	row, col  int
	hovered   bool
	viewCalls int
}

func (d *pointerAuditDialog) Init() tea.Cmd        { return nil }
func (d *pointerAuditDialog) Position() (int, int) { return d.row, d.col }
func (d *pointerAuditDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	if motion, ok := msg.(tea.MouseMotionMsg); ok {
		hovered := motion.X > d.col && motion.X < d.col+9 && motion.Y == d.row+5
		if hovered != d.hovered {
			d.hovered = hovered
			d.InvalidateView()
		}
	}
	return d, nil
}

func (d *pointerAuditDialog) View() string {
	d.viewCalls++
	state := "idle"
	if d.hovered {
		state = "hovered"
	}
	return "┌──────────┐\n│ audit    │\n│          │\n│          │\n│          │\n│ " + state + " │\n└──────────┘"
}

func TestNoOpPointerAndWheelReuseRootCache(t *testing.T) {
	root := wallClockRoot(t, 120, 40)
	root.focusedPanel = PanelContent
	_ = root.chatPage.FocusMessages()
	_, _ = root.Update(tea.KeyPressMsg{Code: 'g'}) // top boundary
	first := root.View()
	for range 100 {
		_, _ = root.Update(messages.PointerUpdateMsg{HasWheel: true, WheelDelta: -1, X: 1, Y: 1})
		if !root.viewCacheValid || root.viewCache.Content != first.Content {
			t.Fatal("top-boundary wheel did not restore the cached root view")
		}
		if got := root.View(); got.Content != first.Content {
			t.Fatal("top-boundary wheel changed the cached root view")
		}
	}
	_, _ = root.Update(messages.PointerUpdateMsg{Motion: &tea.MouseMotionMsg{X: 0, Y: root.height - 1}})
	first = root.View()
	for range 100 {
		_, _ = root.Update(messages.PointerUpdateMsg{Motion: &tea.MouseMotionMsg{X: 0, Y: root.height - 1}})
		if !root.viewCacheValid || root.viewCache.Content != first.Content {
			t.Fatal("identical no-op motion did not restore the cached root view")
		}
		if got := root.View(); got.Content != first.Content {
			t.Fatal("identical no-op motion changed the cached root view")
		}
	}

	invalidUpdates := []messages.PointerUpdateMsg{
		{},
		{WheelDelta: -1},
		{HasWheel: true, Motion: &tea.MouseMotionMsg{X: 1, Y: 1}},
	}
	for _, msg := range invalidUpdates {
		root.viewCache = first
		root.viewCacheValid = true
		_, _ = root.Update(msg)
		if root.viewCacheValid {
			t.Fatalf("invalid pointer update restored the root cache: %#v", msg)
		}
	}
	root.ar.Stop()
}

func TestHarmlessMotionPreservesComposedScrollviews(t *testing.T) {
	t.Run("messages", func(t *testing.T) {
		root := populateScrollableRoot(t)
		motion := tea.MouseMotionMsg{X: 1, Y: 1}
		_, _ = root.Update(messages.PointerUpdateMsg{Motion: &motion})
		cached := root.View()
		before := root.chatPage.VisualGeneration()
		_, _ = root.Update(messages.PointerUpdateMsg{Motion: &motion})
		require.Equal(t, before, root.chatPage.VisualGeneration())
		require.True(t, root.viewCacheValid)
		requireSameView(t, cached, root.viewCache)
		requireSameView(t, cached, root.View())
		root.ar.Stop()
	})

	t.Run("sidebar", func(t *testing.T) {
		sess := &session.Session{ID: "sidebar-motion-cache", Title: "sidebar-motion-cache"}
		a := app.New(t.Context(), stubRuntime{}, sess)
		root := wallClockRoot(t, 160, 24)
		root.hideSidebar = false
		ss := service.NewSessionState(sess)
		ss.SetCurrentAgentName("agent-00")
		page := pagechat.New(root.ar, t.Context(), a, ss)
		page.SetRoutingID(sess.ID)
		root.application, root.chatPage, root.sessionState = a, page, ss
		root.chatPages[sess.ID], root.sessionStates[sess.ID] = page, ss
		root.handleWindowResize(160, 24)
		_ = root.Init()
		agents := make([]runtime.AgentDetails, 30)
		for i := range agents {
			agents[i] = runtime.AgentDetails{
				Name:        fmt.Sprintf("agent-%02d", i),
				Provider:    "provider",
				Model:       "model",
				Description: "sidebar content",
			}
		}
		_, _ = root.Update(runtime.TeamInfo(agents, "agent-00"))
		cached := root.View()
		before := sidebarVisualGeneration(root.chatPage)
		motion := tea.MouseMotionMsg{X: 150, Y: 1}
		_, _ = root.Update(messages.PointerUpdateMsg{Motion: &motion})
		require.Equal(t, before, sidebarVisualGeneration(root.chatPage))
		require.True(t, root.viewCacheValid)
		requireSameView(t, cached, root.viewCache)
		requireSameView(t, cached, root.View())
		root.ar.Stop()
	})
}
