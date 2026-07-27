package dialog

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

type wheelRecordingDialog struct{ got tea.Msg }

func (d *wheelRecordingDialog) Init() tea.Cmd { return nil }
func (d *wheelRecordingDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	d.got = msg
	return d, nil
}
func (d *wheelRecordingDialog) View() string             { return "dialog" }
func (d *wheelRecordingDialog) Position() (int, int)     { return 5, 5 }
func (d *wheelRecordingDialog) SetSize(int, int) tea.Cmd { return nil }

func TestManagerAdjustsCoalescedWheelForDraggedDialog(t *testing.T) {
	d := &wheelRecordingDialog{}
	mgr := New().(*manager)
	_, _ = mgr.Update(OpenDialogMsg{Model: d})
	mgr.stack[0].offsetX, mgr.stack[0].offsetY = 3, 4
	_, _ = mgr.Update(messages.WheelCoalescedMsg{Delta: 2, X: 20, Y: 30})
	got, ok := d.got.(messages.WheelCoalescedMsg)
	require.True(t, ok)
	require.Equal(t, messages.WheelCoalescedMsg{Delta: 2, X: 17, Y: 26}, got)
}
