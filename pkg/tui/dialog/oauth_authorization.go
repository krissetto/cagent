//nolint:gocritic // Dialog command returns intentionally preserve Bubble Tea evaluation shape.
package dialog

import (
	"context"
	"fmt"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

type oauthAuthorizationDialog struct {
	BaseDialog

	ctx func() context.Context

	serverURL     string
	elicitationID string
	app           *app.App
	keyMap        ConfirmKeyMap
}

// NewOAuthAuthorizationDialog creates a new OAuth authorization confirmation dialog.
func NewOAuthAuthorizationDialog(ctx context.Context, serverURL string, appInstance *app.App, elicitationID string) Dialog {
	return &oauthAuthorizationDialog{
		ctx:           func() context.Context { return context.WithoutCancel(ctx) },
		serverURL:     serverURL,
		elicitationID: elicitationID,
		app:           appInstance,
		keyMap:        DefaultConfirmKeyMap(),
	}
}

// Init initializes the OAuth authorization confirmation dialog
func (d *oauthAuthorizationDialog) Init() tea.Cmd {
	return nil
}

// CancelDialogCmd declines exactly like the No key, atomically closing
// the dialog and answering the runtime waiter.
func (d *oauthAuthorizationDialog) CancelDialogCmd() tea.Cmd {
	return d.respond(tools.ElicitationActionDecline)
}

func (d *oauthAuthorizationDialog) OutsideClickDismissCmd() tea.Cmd {
	return d.CancelDialogCmd()
}

func (d *oauthAuthorizationDialog) respond(action tools.ElicitationAction) tea.Cmd {
	return CloseWithElicitationResponse(action, nil, d.elicitationID)
}

func (d *oauthAuthorizationDialog) layout() DialogLayout {
	view := d.View()
	row, col := d.CenterDialog(view)
	return NewDialogLayout(view, row, col)
}

// Update handles messages for the OAuth authorization confirmation dialog
func (d *oauthAuthorizationDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		cmd := d.SetSize(msg.Width, msg.Height)
		return d, cmd

	case tea.MouseClickMsg:
		if msg.Button == tea.MouseLeft && d.CloseButtonHit(msg, d.layout()) {
			return d, d.respond(tools.ElicitationActionDecline)
		}

	case tea.MouseMotionMsg:
		d.HandleMouseMotion(msg.X, msg.Y, d.layout())
		return d, nil

	case tea.KeyPressMsg:
		if cmd := HandleQuit(msg); cmd != nil {
			return d, cmd
		}

		model, cmd, handled := HandleConfirmKeys(msg, d.keyMap,
			func() (layout.Model, tea.Cmd) {
				return d, d.respond(tools.ElicitationActionAccept)
			},
			func() (layout.Model, tea.Cmd) {
				return d, d.respond(tools.ElicitationActionDecline)
			},
		)
		if handled {
			return model, cmd
		}
	}

	return d, nil
}

// Position returns the dialog position (centered)
func (d *oauthAuthorizationDialog) Position() (row, col int) {
	return d.CenterDialog(d.View())
}

// View renders the OAuth authorization confirmation dialog
func (d *oauthAuthorizationDialog) View() string {
	dialogWidth := d.ComputeDialogWidth(60, 40, 90)
	contentWidth := d.ContentWidth(dialogWidth, 2)

	serverInfo := styles.InfoStyle.
		Width(contentWidth).
		Render(fmt.Sprintf("Server: %s (remote)", d.serverURL))

	description := styles.DialogContentStyle.
		Width(contentWidth).
		Render("This server requires OAuth authentication to access its tools. Your browser will open automatically to complete the authorization process.")

	instructions := "After authorizing in your browser, return here and the agent will continue automatically."

	content := NewContent(contentWidth).
		AddContent(styles.DialogTitleInfoStyle.Width(contentWidth).Render("\U0001F510 OAuth Authorization Required")).
		AddSpace().
		AddContent(serverInfo).
		AddSpace().
		AddContent(description).
		AddSpace().
		AddHelp(instructions).
		AddSpace().
		AddHelpKeys("Y", "authorize", "N", "decline").
		Build()

	return d.RenderCard(styles.DialogWarningStyle, dialogWidth, content)
}
