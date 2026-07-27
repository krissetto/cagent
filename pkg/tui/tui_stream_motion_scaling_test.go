package tui

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	agentruntime "github.com/docker/docker-agent/pkg/runtime"
	messagecomponent "github.com/docker/docker-agent/pkg/tui/components/messages"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

type (
	streamingMotionAck   struct{ done chan struct{} }
	streamingMotionRead  struct{ content chan string }
	streamingMotionModel struct {
		root                *appModel
		ready               chan struct{}
		once                sync.Once
		chunks, motions     atomic.Uint64
		views, compositions atomic.Uint64
	}
)

func (m *streamingMotionModel) Init() tea.Cmd { return nil }
func (m *streamingMotionModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case *agentruntime.AgentChoiceEvent:
		m.chunks.Add(1)
	case tea.MouseMotionMsg:
		m.motions.Add(1)
	case streamingMotionAck:
		close(msg.done)
		return m, nil
	case streamingMotionRead:
		msg.content <- m.root.View().Content
		return m, nil
	}
	updated, cmd := m.root.Update(msg)
	m.root = updated.(*appModel)
	return m, cmd
}

func (m *streamingMotionModel) View() tea.View {
	m.views.Add(1)
	if !m.root.viewCacheValid {
		m.compositions.Add(1)
	}
	v := m.root.View()
	m.once.Do(func() { close(m.ready) })
	return v
}

func TestActualProgramLongStreamMotionWorkIsViewportBounded(t *testing.T) {
	root, _, _ := wallClockRoot(t, 120, 40)
	sess, _, _ := mixedHistorySession(1000)
	root.application.Session().Messages = sess.Messages
	_ = root.chatPage.Init()
	root.handleWindowResize(120, 40)
	_, _ = root.Update(messages.RoutedMsg{SessionID: "profile", Inner: agentruntime.StreamStarted("profile", "root")})
	_ = root.View()
	work := root.chatPage.(interface {
		ResetWorkCountersForTest()
		WorkCountersForTest() messagecomponent.WorkCounters
	})
	model := &streamingMotionModel{root: root, ready: make(chan struct{})}
	writer := &wallClockCountingWriter{}
	program := tea.NewProgram(model, tea.WithInput(nil), tea.WithOutput(writer), tea.WithWindowSize(120, 40))
	done := make(chan error, 1)
	go func() { _, err := program.Run(); done <- err }()
	<-model.ready
	time.Sleep(20 * time.Millisecond) //nolint:forbidigo // Deliberately allow the Bubble Tea program event loop to flush.

	chunk := "Paragraph with **markdown**, `code`, Unicode λ界, and a [link](https://example.com).\n\n"
	var early, late messagecomponent.WorkCounters
	for i := range 240 {
		program.Send(agentruntime.AgentChoice("root", "profile", chunk))
		program.Send(tea.MouseMotionMsg{X: 50 + i%2, Y: 20})
		if i == 39 || i == 199 {
			ack := make(chan struct{})
			program.Send(streamingMotionAck{done: ack})
			<-ack
			if i == 39 {
				early = work.WorkCountersForTest()
				work.ResetWorkCountersForTest()
			} else {
				late = work.WorkCountersForTest()
			}
		}
	}
	ack := make(chan struct{})
	program.Send(streamingMotionAck{done: ack})
	<-ack
	time.Sleep(30 * time.Millisecond) //nolint:forbidigo // Deliberately allow the Bubble Tea program event loop to flush.
	require.Equal(t, uint64(240), model.chunks.Load())
	require.Equal(t, uint64(240), model.motions.Load())
	require.LessOrEqual(t, late.RenderedViews, early.RenderedViews*6+20, "late stream work must not scale with accumulated history/content: early=%+v late=%+v", early, late)
	require.LessOrEqual(t, late.HistoryTraversals, early.HistoryTraversals*6+20)
	require.Positive(t, writer.writes.Load(), "stream remains writer-visible")
	views := model.views.Load()
	time.Sleep(30 * time.Millisecond) //nolint:forbidigo // Deliberately allow the Bubble Tea program event loop to flush.
	require.Equal(t, views, model.views.Load(), "program quiesces after stream input")
	program.Quit()
	require.NoError(t, <-done)
	root.animationRuntime.Stop()
}
