package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	agentruntime "github.com/docker/docker-agent/pkg/runtime"
	messagecomponent "github.com/docker/docker-agent/pkg/tui/components/messages"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func TestBlurFocusDoesNotMaterializeStreamTail(t *testing.T) {
	root, _, _ := wallClockRoot(t, 120, 40)
	_, _ = root.Update(messages.RoutedMsg{SessionID: "profile", Inner: agentruntime.StreamStarted("profile", "root")})
	chunk := "Paragraph with **markdown**, Unicode λ界, and a [link](https://example.com).\n\n"
	for range 100 {
		_, _ = root.Update(messages.RoutedMsg{SessionID: "profile", Inner: agentruntime.AgentChoice("root", "profile", chunk)})
		_ = root.View()
	}
	_, _ = root.Update(messages.WheelCoalescedMsg{Delta: -3, X: 40, Y: 20})
	for range 20 {
		_, _ = root.Update(messages.RoutedMsg{SessionID: "profile", Inner: agentruntime.AgentChoice("root", "profile", chunk)})
	}
	probe := root.chatPage.(interface {
		GeometryForTest() messagecomponent.GeometryForTest
		ResetWorkCountersForTest()
		WorkCountersForTest() messagecomponent.WorkCounters
	})
	before := probe.GeometryForTest()
	probe.ResetWorkCountersForTest()
	_, _ = root.Update(tea.BlurMsg{})
	_, _ = root.Update(tea.FocusMsg{})
	require.Equal(t, before, probe.GeometryForTest())
	require.Zero(t, probe.WorkCountersForTest().Materializations)
	root.animationRuntime.Stop()
}

func programFrame(t *testing.T, program *tea.Program) string {
	t.Helper()
	content := make(chan string)
	program.Send(streamingMotionRead{content: content})
	return <-content
}

func programAck(t *testing.T, program *tea.Program) {
	t.Helper()
	ack := make(chan struct{})
	program.Send(streamingMotionAck{done: ack})
	<-ack
}

func TestActualProgramPendingSpinnerHoverIsFrameIsolated(t *testing.T) {
	root, _, _ := wallClockRoot(t, 120, 40)
	_, _ = root.Update(messages.RoutedMsg{SessionID: "profile", Inner: agentruntime.StreamStarted("profile", "root")})
	model := &streamingMotionModel{root: root, ready: make(chan struct{})}
	program := tea.NewProgram(model, tea.WithInput(nil), tea.WithOutput(&wallClockCountingWriter{}), tea.WithWindowSize(120, 40))
	done := make(chan error, 1)
	go func() { _, err := program.Run(); done <- err }()
	<-model.ready

	baseline := programFrame(t, program)
	baselineGeometry := root.chatPage.(interface {
		GeometryForTest() messagecomponent.GeometryForTest
	}).GeometryForTest()
	require.NotEmpty(t, strings.TrimSpace(ansi.Strip(baseline)), "pending spinner frame")
	for range 20 {
		program.Send(tea.MouseMotionMsg{X: 40, Y: 25})
		programAck(t, program)
		hovered := programFrame(t, program)
		require.NotEmpty(t, strings.TrimSpace(ansi.Strip(hovered)), "hover produced an intermediate blank frame")
		require.Equal(t, rootFrameWidths(baseline), rootFrameWidths(hovered), "same elapsed animation state geometry")
		require.Equal(t, baselineGeometry, root.chatPage.(interface {
			GeometryForTest() messagecomponent.GeometryForTest
		}).GeometryForTest())
		program.Send(tea.MouseMotionMsg{X: 40, Y: 2})
		programAck(t, program)
		require.Equal(t, baseline, programFrame(t, program), "leave restores the exact same-elapsed frame")
	}
	program.Quit()
	require.NoError(t, <-done)
	root.animationRuntime.Stop()
}

func TestActualProgramVirtualSuffixKeyWheelPageMatrixNeverBlanks(t *testing.T) {
	root, _, _ := wallClockRoot(t, 120, 40)
	_, _ = root.Update(messages.RoutedMsg{SessionID: "profile", Inner: agentruntime.StreamStarted("profile", "root")})
	chunk := "stream marker **bold** `code` λ界\n\n"
	for range 200 {
		_, _ = root.Update(messages.RoutedMsg{SessionID: "profile", Inner: agentruntime.AgentChoice("root", "profile", chunk)})
		_ = root.View()
	}
	model := &streamingMotionModel{root: root, ready: make(chan struct{})}
	program := tea.NewProgram(model, tea.WithInput(nil), tea.WithOutput(&wallClockCountingWriter{}), tea.WithWindowSize(120, 40))
	done := make(chan error, 1)
	go func() { _, err := program.Run(); done <- err }()
	<-model.ready

	for _, msg := range []tea.Msg{
		messages.WheelCoalescedMsg{Delta: -1_000_000, X: 40, Y: 20},
		tea.KeyPressMsg{Code: tea.KeyPgDown},
		tea.KeyPressMsg{Code: 'G'},
		tea.KeyPressMsg{Code: tea.KeyPgUp},
		messages.WheelCoalescedMsg{Delta: 1_000_000, X: 40, Y: 20},
	} {
		program.Send(msg)
		programAck(t, program)
		frame := programFrame(t, program)
		require.NotEmpty(t, strings.TrimSpace(ansi.Strip(frame)), "movement produced a blank root frame: %T", msg)
		lineCount := len(strings.Split(frame, "\n"))
		require.Contains(t, []int{39, 40}, lineCount, "fixed root height with optional terminal trailing newline after %T", msg)
		for _, width := range rootFrameWidths(frame) {
			require.Equal(t, 120, width, "fixed root width after %T", msg)
		}
	}
	frame := programFrame(t, program)
	require.Contains(t, ansi.Strip(frame), "stream marker", "exact bottom dropped virtual active suffix")
	program.Quit()
	require.NoError(t, <-done)
	root.animationRuntime.Stop()
}

func TestActualProgramHoverThenBottomReentryStaysBounded(t *testing.T) {
	root, _, _ := wallClockRoot(t, 120, 40)
	_, _ = root.Update(messages.RoutedMsg{SessionID: "profile", Inner: agentruntime.StreamStarted("profile", "root")})
	chunk := "Paragraph with **markdown**, `code`, Unicode λ界, and a [link](https://example.com).\n\n"
	for range 300 {
		_, _ = root.Update(messages.RoutedMsg{SessionID: "profile", Inner: agentruntime.AgentChoice("root", "profile", chunk)})
		_ = root.View()
	}
	root.chatPage.ScrollToBottom()
	_ = root.View()
	probe := root.chatPage.(interface {
		GeometryForTest() messagecomponent.GeometryForTest
		ResetWorkCountersForTest()
		WorkCountersForTest() messagecomponent.WorkCounters
	})
	before := probe.GeometryForTest()
	beforeFrame := root.View().Content

	model := &streamingMotionModel{root: root, ready: make(chan struct{})}
	writer := &wallClockCountingWriter{}
	program := tea.NewProgram(model, tea.WithInput(nil), tea.WithOutput(writer), tea.WithWindowSize(120, 40))
	done := make(chan error, 1)
	go func() { _, err := program.Run(); done <- err }()
	<-model.ready
	program.Send(tea.MouseMotionMsg{X: 40, Y: 25})
	ack := make(chan struct{})
	program.Send(streamingMotionAck{done: ack})
	<-ack
	after := probe.GeometryForTest()
	require.Equal(t, before, after, "hover must not change transcript/viewport geometry or follow state")
	require.Len(t, root.View().Content, len(beforeFrame), "hover control occupies reserved space")

	program.Send(messages.WheelCoalescedMsg{Delta: -3, X: 40, Y: 20})
	for range 100 {
		program.Send(agentruntime.AgentChoice("root", "profile", chunk))
	}
	ack = make(chan struct{})
	program.Send(streamingMotionAck{done: ack})
	<-ack
	probe.ResetWorkCountersForTest()
	program.Send(messages.WheelCoalescedMsg{Delta: 1_000_000, X: 40, Y: 20})
	ack = make(chan struct{})
	program.Send(streamingMotionAck{done: ack})
	<-ack
	reentry := probe.WorkCountersForTest()
	require.Equal(t, uint64(1), reentry.Materializations)
	require.LessOrEqual(t, reentry.RenderedViews, uint64(1))
	require.False(t, probe.GeometryForTest().UserHasScrolled)

	probe.ResetWorkCountersForTest()
	for range 20 {
		program.Send(agentruntime.AgentChoice("root", "profile", chunk))
	}
	ack = make(chan struct{})
	program.Send(streamingMotionAck{done: ack})
	<-ack
	time.Sleep(20 * time.Millisecond) //nolint:forbidigo // Deliberately allow the Bubble Tea program event loop to flush.
	follow := probe.WorkCountersForTest()
	require.Zero(t, follow.Materializations)
	require.LessOrEqual(t, follow.RenderedViews, uint64(20))
	program.Quit()
	require.NoError(t, <-done)
	root.animationRuntime.Stop()
}
