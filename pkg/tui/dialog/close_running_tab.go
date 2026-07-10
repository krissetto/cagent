package dialog

import (
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// CloseRunningTabConfirmedMsg is sent when the user confirms closing a tab
// that still has an active stream.
type CloseRunningTabConfirmedMsg struct {
	SessionID string
}

type closeRunningTabKeyMap struct {
	Yes key.Binding
	No  key.Binding
	Esc key.Binding
}

func defaultCloseRunningTabKeyMap() closeRunningTabKeyMap {
	return closeRunningTabKeyMap{
		Yes: key.NewBinding(
			key.WithKeys("y", "Y"),
			key.WithHelp("Y", "close"),
		),
		No: key.NewBinding(
			key.WithKeys("n", "N"),
			key.WithHelp("N", "cancel"),
		),
		Esc: key.NewBinding(
			key.WithKeys("esc"),
			key.WithHelp("Esc", "cancel"),
		),
	}
}

type closeRunningTabDialog struct {
	BaseDialog

	sessionID string
	keyMap    closeRunningTabKeyMap
}

// NewCloseRunningTabDialog creates a confirmation dialog for closing a tab
// whose stream is still active.
func NewCloseRunningTabDialog(sessionID string) Dialog {
	return &closeRunningTabDialog{
		sessionID: sessionID,
		keyMap:    defaultCloseRunningTabKeyMap(),
	}
}

func (d *closeRunningTabDialog) Init() tea.Cmd { return nil }

func (d *closeRunningTabDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		cmd := d.SetSize(msg.Width, msg.Height)
		return d, cmd

	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, d.keyMap.Yes):
			return d, tea.Sequence(
				core.CmdHandler(CloseDialogMsg{}),
				core.CmdHandler(CloseRunningTabConfirmedMsg{SessionID: d.sessionID}),
			)
		case key.Matches(msg, d.keyMap.No), key.Matches(msg, d.keyMap.Esc):
			return d, core.CmdHandler(CloseDialogMsg{})
		}
	}

	return d, nil
}

func (d *closeRunningTabDialog) Position() (row, col int) {
	return d.CenterDialog(d.View())
}

func (d *closeRunningTabDialog) View() string {
	dialogWidth := d.ComputeDialogWidth(50, 34, 58)
	contentWidth := d.ContentWidth(dialogWidth, 2)

	content := NewContent(contentWidth).
		AddTitle("Close Running Tab").
		AddSeparator().
		AddSpace().
		AddQuestion("This tab has an active response. Close it anyway?").
		AddSpace().
		AddHelpKeys("Y", "close", "N", "cancel").
		Build()

	return styles.DialogStyle.
		Padding(1, 2).
		Width(dialogWidth).
		Render(content)
}
