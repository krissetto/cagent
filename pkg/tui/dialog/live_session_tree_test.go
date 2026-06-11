package dialog

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func TestLiveSessionTreeDialogEnterAttachesSelectedChild(t *testing.T) {
	t.Parallel()

	d := NewLiveSessionTreeDialog(&runtime.LiveSessionTree{Root: &runtime.LiveSessionNode{
		ID:       "root",
		Children: []*runtime.LiveSessionNode{{ID: "child-a", AgentName: "reviewer", Depth: 1}},
	}})

	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, cmd)
	msg := cmd()
	attach, ok := msg.(messages.AttachSessionMsg)
	require.True(t, ok)
	require.Equal(t, "child-a", attach.SessionID)
}
