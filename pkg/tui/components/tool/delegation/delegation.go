package delegation

import (
	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/components/spinner"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

type delegationCard struct {
	msg               *types.Message
	spinner           spinner.Spinner
	width             int
	spinnerRegistered bool
}

func New(msg *types.Message, _ service.SessionStateReader) layout.Model {
	return &delegationCard{
		msg:     msg,
		spinner: spinner.New(spinner.ModeSpinnerOnly, styles.SpinnerDotsAccentStyle),
		width:   80,
	}
}

func (d *delegationCard) Init() tea.Cmd {
	if !d.msg.DelegationDone {
		d.spinnerRegistered = true
		return d.spinner.Init()
	}
	return nil
}

func (d *delegationCard) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	if d.spinnerRegistered && !d.msg.DelegationDone {
		updated, cmd := d.spinner.Update(msg)
		d.spinner = updated.(spinner.Spinner)
		return d, cmd
	}
	return d, nil
}

func (d *delegationCard) View() string {
	msg := d.msg

	var icon string
	switch {
	case msg.DelegationFailed:
		icon = styles.ToolErrorIcon.Render("✗")
	case msg.DelegationDone:
		icon = styles.ToolCompletedIcon.Render("✓")
	default:
		icon = styles.NoStyle.MarginLeft(2).Render(d.spinner.View())
	}

	agentBadge := styles.AgentBadgeStyleFor(msg.DelegationAgent).Render(msg.DelegationAgent)

	// Single-line compact pill: ✓  planner
	// No task preview — the user can open the session from the sidebar.
	return icon + "  " + agentBadge
}

func (d *delegationCard) SetSize(width, height int) tea.Cmd {
	d.width = width
	return nil
}

func (d *delegationCard) GetSize() (int, int) {
	return d.width, 2
}

func (d *delegationCard) StopAnimation() {
	if d.spinnerRegistered {
		d.spinner.Stop()
		d.spinnerRegistered = false
	}
}
