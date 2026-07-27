package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	agentruntime "github.com/docker/docker-agent/pkg/runtime"
	messagecomponent "github.com/docker/docker-agent/pkg/tui/components/messages"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func visibleGeometry(g messagecomponent.GeometryForTest) [7]int {
	return [7]int{g.Width, g.Height, g.ContentWidth, g.TotalHeight, g.ScrollOffset, g.MaxOffset, g.ScrollbarX}
}

func TestActualProgramScrollCancelResizeMatrixNeverNeedsRecoveryClick(t *testing.T) {
	root, _, _ := wallClockRoot(t, 120, 40)
	_, _ = root.Update(messages.RoutedMsg{SessionID: "profile", Inner: agentruntime.StreamStarted("profile", "root")})
	_, _ = root.Update(messages.RoutedMsg{SessionID: "profile", Inner: agentruntime.AgentChoiceReasoning("root", "profile", "thinking prefix λ界\n\n")})
	chunk := "matrix marker **bold** `code` λ界 [link](https://example.com)\n\n"
	for range 120 {
		_, _ = root.Update(messages.RoutedMsg{SessionID: "profile", Inner: agentruntime.AgentChoice("root", "profile", chunk)})
		_ = root.View()
	}
	model := &streamingMotionModel{root: root, ready: make(chan struct{})}
	recorder := newTerminalRecorder(120, 40)
	program := tea.NewProgram(model, tea.WithInput(nil), tea.WithOutput(recorder), tea.WithWindowSize(120, 40))
	done := make(chan error, 1)
	go func() { _, err := program.Run(); done <- err }()
	<-model.ready

	sequence := []tea.Msg{
		messages.WheelCoalescedMsg{Delta: -1_000_000, X: 40, Y: 20},
		tea.MouseMotionMsg{X: 40, Y: 20},
		tea.BlurMsg{},
		tea.FocusMsg{},
		tea.WindowSizeMsg{Width: 1, Height: 1},
		tea.WindowSizeMsg{Width: 120, Height: 40},
		messages.WheelCoalescedMsg{Delta: 4, X: 40, Y: 20},
		messages.StreamCancelledMsg{ShowMessage: true},
		messages.WheelCoalescedMsg{Delta: 1_000_000, X: 40, Y: 20},
	}
	for i, msg := range sequence {
		program.Send(msg)
		programAck(t, program)
		before := programFrame(t, program)
		beforeRaw, beforeScreen := recorder.snapshot()
		beforeGeometry := root.chatPage.(interface {
			GeometryForTest() messagecomponent.GeometryForTest
		}).GeometryForTest()
		beforeContent := root.chatPage.(interface{ LastMessageContentForTest() string }).LastMessageContentForTest()
		beforeValid := root.viewCacheValid

		// Mandatory inert click recovery probe at every transition.
		program.Send(tea.MouseClickMsg{Button: tea.MouseLeft, X: 0, Y: 39})
		programAck(t, program)
		after := programFrame(t, program)
		afterRaw, afterScreen := recorder.snapshot()
		afterGeometry := root.chatPage.(interface {
			GeometryForTest() messagecomponent.GeometryForTest
		}).GeometryForTest()
		afterContent := root.chatPage.(interface{ LastMessageContentForTest() string }).LastMessageContentForTest()
		t.Logf("step=%d event=%T cache=%v geom-before=%+v geom-after=%+v widths=%v rootRowsStartingScrollbar=%d terminalRowsStartingScrollbar=%d rawBefore=%d rawAfter=%d", i, msg, beforeValid, beforeGeometry, afterGeometry, rootFrameWidths(before), rowsStartingScrollbar(ansi.Strip(before)), rowsStartingScrollbar(beforeScreen), len(beforeRaw), len(afterRaw))
		require.NotEmpty(t, strings.TrimSpace(ansi.Strip(before)), "blank frame before recovery click at %T", msg)
		require.Equal(t, visibleGeometry(beforeGeometry), visibleGeometry(afterGeometry), "inert click changed visible geometry at %T", msg)
		require.Equal(t, beforeContent, afterContent, "inert click changed content at %T", msg)
		require.Equal(t, ansi.Strip(before), ansi.Strip(after), "inert click repaired stale frame at %T", msg)
		_ = afterScreen // terminal state is inspected/logged; renderer may flush asynchronously after the ack.
	}
	program.Quit()
	require.NoError(t, <-done)
	root.animationRuntime.Stop()
}
