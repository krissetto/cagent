package dialog

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestExitConfirmationChromeSnapshot(t *testing.T) {
	d := NewExitConfirmationDialog().(*exitConfirmationDialog)
	d.SetSize(80, 24)
	view := ansi.Strip(d.View())
	assert.Contains(t, view, dialogCloseGlyph)
	assert.Contains(t, view, "No ↵")
	assert.Contains(t, view, "Yes")
	assert.NotContains(t, view, "Y yes")
}

func TestExitConfirmationKeyboardParity(t *testing.T) {
	d := NewExitConfirmationDialog().(*exitConfirmationDialog)
	d.SetSize(80, 24)

	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, cmd)
	_, ok := cmd().(CloseDialogMsg)
	assert.True(t, ok, "Enter activates the default No pill")

	_, _ = d.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	assert.Contains(t, ansi.Strip(d.View()), "Yes ↵")
	_, cmd = d.Update(tea.KeyPressMsg{Text: "n"})
	require.NotNil(t, cmd)
	_, ok = cmd().(CloseDialogMsg)
	assert.True(t, ok, "N remains a direct keyboard shortcut")
}

func TestExitConfirmationPillAndCloseHitboxes(t *testing.T) {
	d := NewExitConfirmationDialog().(*exitConfirmationDialog)
	d.SetSize(80, 24)
	dl := d.layout()

	closeClick := tea.MouseClickMsg{Button: tea.MouseLeft, X: dl.Col + dl.Width - 3, Y: dl.Row + 1}
	_, cmd := d.Update(closeClick)
	require.NotNil(t, cmd)
	_, ok := cmd().(CloseDialogMsg)
	assert.True(t, ok)

	lines := strings.Split(ansi.Strip(dl.View), "\n")
	buttonY := -1
	for i, line := range lines {
		if strings.Contains(line, "No ↵") && strings.Contains(line, "Yes") {
			buttonY = dl.Row + i
		}
	}
	require.NotEqual(t, -1, buttonY)
	style := stylesForTest()
	contentLeft := dl.Col + style.GetBorderLeftSize() + style.GetPaddingLeft()
	_, cmd = d.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: contentLeft + d.confirmBtnNoX, Y: buttonY})
	require.NotNil(t, cmd)
	_, ok = cmd().(CloseDialogMsg)
	assert.True(t, ok, "entire No pill is clickable from its first cell")
}

func TestManagerCloseHoverIsScopedToDialogLifecycle(t *testing.T) {
	mgr := New().(*manager)
	mgr.SetSize(80, 24)

	a := NewExitConfirmationDialog()
	_, _ = mgr.Update(OpenDialogMsg{Model: a})
	row, col := a.Position()
	closeX := col + lipgloss.Width(a.View()) - styles.DialogStyle.GetBorderRightSize() - 1 - dialogCloseInset
	closeY := row + styles.DialogStyle.GetBorderTopSize()
	_, _ = mgr.Update(tea.MouseMotionMsg{X: closeX, Y: closeY})
	require.True(t, mgr.stack[len(mgr.stack)-1].closeHovered)
	require.True(t, a.(*exitConfirmationDialog).closeHovered)

	_, closeCmd := mgr.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: closeX, Y: closeY})
	require.NotNil(t, closeCmd)
	_, _ = mgr.Update(closeCmd())

	b := NewExitConfirmationDialog()
	// Even a reused model carrying stale component-local state starts a new
	// open lifecycle unhovered, without requiring synthetic pointer motion.
	b.(*exitConfirmationDialog).closeHovered = true
	_, _ = mgr.Update(OpenDialogMsg{Model: b})
	top := &mgr.stack[len(mgr.stack)-1]
	assert.False(t, top.closeHovered)
	assert.False(t, b.(*exitConfirmationDialog).closeHovered)

	row, col = b.Position()
	unhoveredB := b.View()
	closeX = col + lipgloss.Width(unhoveredB) - styles.DialogStyle.GetBorderRightSize() - 1 - dialogCloseInset
	closeY = row + styles.DialogStyle.GetBorderTopSize()
	_, _ = mgr.Update(tea.MouseMotionMsg{X: closeX, Y: closeY})
	assert.NotEqual(t, unhoveredB, b.View(), "fresh B motion activates close hover visually")
	assert.True(t, mgr.stack[len(mgr.stack)-1].closeHovered, "fresh B motion activates manager hover")
	assert.True(t, b.(*exitConfirmationDialog).closeHovered, "fresh B motion activates component hover")
}

func TestManagerCloseButtonWinsBeforeDrag(t *testing.T) {
	mgr := New().(*manager)
	mgr.SetSize(80, 24)
	d := NewExitConfirmationDialog()
	d.SetSize(80, 24)
	mgr.stack = []dialogEntry{{dialog: d}}
	dl := d.(*exitConfirmationDialog).layout()

	_, cmd := mgr.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: dl.Col + dl.Width - 3, Y: dl.Row + 1})
	require.NotNil(t, cmd)
	_, ok := cmd().(CloseDialogMsg)
	assert.True(t, ok)
	assert.False(t, mgr.drag.active, "close control must not initiate title dragging")
}

func TestManagerDragClampsDialogToViewport(t *testing.T) {
	mgr := New().(*manager)
	mgr.SetSize(20, 10)
	d := &chromeTestDialog{}
	mgr.stack = []dialogEntry{{dialog: d}}
	mgr.drag = dragState{active: true, startX: 10, startY: 10}

	mgr.handleDragMotion(-100, -100)
	assert.Equal(t, -10, mgr.stack[0].offsetX)
	assert.Equal(t, -10, mgr.stack[0].offsetY)
	mgr.handleDragMotion(100, 100)
	assert.Equal(t, 7, mgr.stack[0].offsetX)
	assert.Equal(t, -1, mgr.stack[0].offsetY)
}

func TestManagerOutsideLeftClickPolicies(t *testing.T) {
	mgr := New().(*manager)
	mgr.SetSize(80, 24)
	regular := &chromeTestDialog{}
	regular.SetSize(80, 24)
	mgr.stack = append(mgr.stack, dialogEntry{dialog: regular})

	_, cmd := mgr.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 0, Y: 0})
	require.NotNil(t, cmd)
	_, ok := cmd().(CloseDialogMsg)
	assert.True(t, ok)

	exit := NewExitConfirmationDialog()
	exit.SetSize(80, 24)
	mgr.stack = []dialogEntry{{dialog: exit}}
	_, cmd = mgr.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 0, Y: 0})
	require.NotNil(t, cmd)
	assert.Equal(t, []tea.Msg{CloseDialogMsg{}}, collectMsgs(cmd),
		"outside left click dismisses exit confirmation without confirming exit")

	_, cmd = mgr.Update(tea.MouseClickMsg{Button: tea.MouseRight, X: 0, Y: 0})
	assert.Nil(t, cmd, "outside policy only applies to left click")
}

func TestExitConfirmationDismissalNeverConfirms(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  tea.Msg
	}{
		{name: "escape", msg: tea.KeyPressMsg{Code: tea.KeyEscape}},
		{name: "ctrl-c", msg: tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}},
		{name: "outside left click", msg: tea.MouseClickMsg{Button: tea.MouseLeft, X: 0, Y: 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr := New().(*manager)
			mgr.SetSize(80, 24)
			d := NewExitConfirmationDialog()
			d.SetSize(80, 24)
			mgr.stack = []dialogEntry{{dialog: d}}

			_, cmd := mgr.Update(tc.msg)
			require.NotNil(t, cmd)
			msgs := collectMsgs(cmd)
			assert.Equal(t, []tea.Msg{CloseDialogMsg{}}, msgs)
			assert.False(t, hasDialogMsg[ExitConfirmedMsg](msgs))
			assert.False(t, hasDialogMsg[tea.QuitMsg](msgs))
		})
	}
}

func hasDialogMsg[T tea.Msg](msgs []tea.Msg) bool {
	for _, msg := range msgs {
		if _, ok := msg.(T); ok {
			return true
		}
	}
	return false
}

func TestRuntimeDialogsOutsideClickPolicy(t *testing.T) {
	tests := []struct {
		name       string
		dialog     Dialog
		wantAction tools.ElicitationAction
		wantID     string
		ignore     bool
	}{
		{
			name:   "tool confirmation remains open",
			dialog: NewToolConfirmationDialog(animation.NewRuntime(), newConfirmationEvent(nil), &service.SessionState{}),
			ignore: true,
		},
		{
			name:       "elicitation cancels",
			dialog:     NewElicitationDialog("question", nil, nil, "form-id"),
			wantAction: tools.ElicitationActionCancel,
			wantID:     "form-id",
		},
		{
			name:       "URL elicitation cancels",
			dialog:     NewURLElicitationDialog(t.Context(), "authorize", "https://example.com", "url-id"),
			wantAction: tools.ElicitationActionCancel,
			wantID:     "url-id",
		},
		{
			name:       "OAuth authorization declines",
			dialog:     NewOAuthAuthorizationDialog(t.Context(), "https://example.com", nil, "oauth-id"),
			wantAction: tools.ElicitationActionDecline,
			wantID:     "oauth-id",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr := New().(*manager)
			mgr.SetSize(100, 40)
			tt.dialog.SetSize(100, 40)
			mgr.stack = []dialogEntry{{dialog: tt.dialog}}

			_, cmd := mgr.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 0, Y: 0})
			if tt.ignore {
				assert.Nil(t, cmd)
				assert.Len(t, mgr.stack, 1, "ignored click must leave the runtime dialog open")
				return
			}

			msgs := collectMsgs(cmd)
			require.Len(t, msgs, 2)
			assert.Equal(t, CloseDialogMsg{}, msgs[0])
			assert.Equal(t, messages.ElicitationResponseMsg{
				Action:        tt.wantAction,
				Content:       nil,
				ElicitationID: tt.wantID,
			}, msgs[1])
		})
	}
}

func TestOAuthDialogManagerCloseXDeclinesAndCloses(t *testing.T) {
	mgr := New().(*manager)
	mgr.SetSize(100, 40)
	d := NewOAuthAuthorizationDialog(t.Context(), "https://example.com", nil, "oauth-id").(*oauthAuthorizationDialog)
	d.SetSize(100, 40)
	mgr.stack = []dialogEntry{{dialog: d}}

	view := d.View()
	assert.Contains(t, ansi.Strip(view), dialogCloseGlyph, "OAuth dialog must render shared close chrome")
	row, col := d.Position()
	closeX := col + lipgloss.Width(view) - styles.DialogWarningStyle.GetBorderRightSize() - 1 - dialogCloseInset
	closeY := row + styles.DialogWarningStyle.GetBorderTopSize()

	_, cmd := mgr.Update(tea.MouseMotionMsg{X: closeX, Y: closeY})
	assert.Nil(t, cmd)
	assert.True(t, d.closeHovered, "manager-routed motion must hover the OAuth close control")

	_, cmd = mgr.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: closeX, Y: closeY})
	require.NotNil(t, cmd)
	msgs := collectMsgs(cmd)
	require.Equal(t, []tea.Msg{
		CloseDialogMsg{},
		messages.ElicitationResponseMsg{
			Action:        tools.ElicitationActionDecline,
			Content:       nil,
			ElicitationID: "oauth-id",
		},
	}, msgs)
	assert.False(t, mgr.drag.active, "close control must not initiate title dragging")

	for _, msg := range msgs {
		_, _ = mgr.Update(msg)
	}
	assert.False(t, mgr.Open(), "close-X command must close the OAuth dialog")
}

func TestURLDialogTitleDragAndCloseX(t *testing.T) {
	newManager := func() (*manager, *URLElicitationDialog) {
		mgr := New().(*manager)
		mgr.SetSize(100, 40)
		d := NewURLElicitationDialog(t.Context(), "authorize", "https://example.com", "url-id").(*URLElicitationDialog)
		d.SetSize(100, 40)
		mgr.stack = []dialogEntry{{dialog: d}}
		return mgr, d
	}

	mgr, d := newManager()
	row, col := d.Position()
	_, cmd := mgr.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: col + 5, Y: row + 2})
	assert.Nil(t, cmd, "title click must start drag instead of opening the URL")
	assert.True(t, mgr.drag.active)

	mgr, d = newManager()
	view := d.View()
	row, col = d.Position()
	closeX := col + lipgloss.Width(view) - styles.DialogStyle.GetBorderRightSize() - 1 - dialogCloseInset
	_, cmd = mgr.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: closeX, Y: row + styles.DialogStyle.GetBorderTopSize()})
	require.NotNil(t, cmd)
	msgs := collectMsgs(cmd)
	require.Len(t, msgs, 2)
	assert.Equal(t, CloseDialogMsg{}, msgs[0])
	assert.Equal(t, messages.ElicitationResponseMsg{
		Action:        tools.ElicitationActionCancel,
		ElicitationID: "url-id",
	}, msgs[1])
	assert.False(t, mgr.drag.active, "close control must win before title dragging")
}

type chromeTestDialog struct{ BaseDialog }

func (d *chromeTestDialog) Init() tea.Cmd                          { return nil }
func (d *chromeTestDialog) Update(tea.Msg) (layout.Model, tea.Cmd) { return d, nil }
func (d *chromeTestDialog) View() string                           { return "box" }
func (d *chromeTestDialog) Position() (int, int)                   { return 10, 10 }

func stylesForTest() lipgloss.Style { return styles.DialogStyle.Padding(1, 2) }

type managerChromeTestDialog struct{ chromeTestDialog }

func (d *managerChromeTestDialog) View() string { return styles.DialogStyle.Width(20).Render("box") }

func TestManagerAddsCloseChromeToPlainDialog(t *testing.T) {
	mgr := New().(*manager)
	mgr.SetSize(40, 12)
	d := &managerChromeTestDialog{}
	d.SetSize(40, 12)
	mgr.stack = []dialogEntry{{dialog: d}}
	assert.Contains(t, ansi.Strip(mgr.View()), dialogCloseGlyph)
}

func TestMandatoryDialogsExplicitlyDisableCloseChrome(t *testing.T) {
	for _, d := range []Dialog{NewMaxIterationsDialog(10, nil), &toolConfirmationDialog{}} {
		policy, ok := d.(ClosePolicy)
		require.True(t, ok)
		assert.False(t, policy.DialogClosable())
	}
}

func TestPickerSizeNeverExceedsRoot(t *testing.T) {
	p := newPickerCore(commandPaletteLayout, "search")
	p.SetSize(24, 6)
	w, h, c := p.dialogSize()
	assert.LessOrEqual(t, w, 24)
	assert.LessOrEqual(t, h, 6)
	assert.GreaterOrEqual(t, c, 1)
	p.SetSize(120, 40)
	w, h, c = p.dialogSize()
	assert.LessOrEqual(t, w, 120)
	assert.LessOrEqual(t, h, 40)
	assert.Greater(t, c, 1)
}
