package tui

import (
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	tuiinput "github.com/docker/docker-agent/pkg/tui/input"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func pointerBenchRoot(tb testing.TB, count int) *appModel {
	tb.Helper()
	root, _, _ := wallClockRoot(tb, 240, 80)
	for i := range count {
		msg := session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: fmt.Sprintf("message %04d https://example.com/%04d\nbody line", i, i)})
		root.application.Session().Messages = append(root.application.Session().Messages, session.NewMessageItem(msg))
	}
	_ = root.chatPage.Init()
	root.handleWindowResize(240, 80)
	root.chatPage.ScrollToBottom()
	root.viewCacheValid = false
	_ = root.View()
	return root
}

type pointerBurstAck struct{ done chan struct{} }

type pointerBurstModel struct {
	root                          *appModel
	ready                         chan struct{}
	once                          sync.Once
	updates, motion, wheel, views atomic.Uint64
	compositions                  atomic.Uint64
}

func (m *pointerBurstModel) Init() tea.Cmd { return nil }

func (m *pointerBurstModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	m.updates.Add(1)
	switch msg := msg.(type) {
	case messages.PointerUpdateMsg:
		if msg.Motion != nil {
			m.motion.Add(1)
		}
		if msg.Wheel {
			m.wheel.Add(1)
		}
	case messages.WheelCoalescedMsg:
		m.wheel.Add(1)
	case pointerBurstAck:
		close(msg.done)
		return m, nil
	}
	updated, cmd := m.root.Update(msg)
	m.root = updated.(*appModel)
	return m, cmd
}

func (m *pointerBurstModel) View() tea.View {
	m.views.Add(1)
	if !m.root.viewCacheValid {
		m.compositions.Add(1)
	}
	view := m.root.View()
	m.once.Do(func() { close(m.ready) })
	return view
}

type pointerBurstMetrics struct {
	raw, updates, motion, wheel, views, compositions uint64
	elapsed, queueDrain                              time.Duration
}

func runPointerBurst(t *testing.T, wheel, motion int) pointerBurstMetrics {
	t.Helper()
	root := pointerBenchRoot(t, 1000)
	model := &pointerBurstModel{root: root, ready: make(chan struct{})}
	controller := tuiinput.NewPointerController()
	program := tea.NewProgram(model, tea.WithInput(nil), tea.WithOutput(io.Discard), tea.WithWindowSize(240, 80), tea.WithFilter(func(_ tea.Model, msg tea.Msg) tea.Msg {
		return controller.Filter(msg)
	}))
	controller.SetSender(program.Send)
	done := make(chan error, 1)
	go func() { _, err := program.Run(); done <- err }()
	<-model.ready

	start := time.Now()
	burstSize := max(wheel, motion)
	for i := range burstSize {
		if i < wheel {
			program.Send(tea.MouseWheelMsg{X: 20, Y: 30, Button: tea.MouseWheelUp})
		}
		if i < motion {
			program.Send(tea.MouseMotionMsg{X: 10 + i%60, Y: 20 + (i/60)%20})
		}
	}
	require.Eventually(t, func() bool {
		if wheel > 0 {
			return model.wheel.Load() > 0
		}
		return model.motion.Load() > 0
	}, time.Second, time.Millisecond)
	queueStart := time.Now()
	ack := make(chan struct{})
	program.Send(pointerBurstAck{done: ack})
	<-ack
	queueDrain := time.Since(queueStart)
	elapsed := time.Since(start)
	program.Quit()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	metrics := pointerBurstMetrics{
		raw: uint64(wheel + motion), updates: model.updates.Load(), motion: model.motion.Load(), wheel: model.wheel.Load(),
		views: model.views.Load(), compositions: model.compositions.Load(), elapsed: elapsed, queueDrain: queueDrain,
	}
	root.animationRuntime.Stop()
	return metrics
}

func TestDeepScrollPointerBurstRemainsLosslessAndBounded(t *testing.T) {
	const events = 1000
	scrollOnly := runPointerBurst(t, events, 0)
	motionOnly := runPointerBurst(t, 0, events)
	combined := runPointerBurst(t, events, events)
	t.Logf("scroll-only: %+v", scrollOnly)
	t.Logf("motion-only: %+v", motionOnly)
	t.Logf("combined: %+v", combined)
	if motionOnly.motion == 0 || motionOnly.motion > 8 {
		t.Fatalf("motion cadence did not retain only latest positions: %+v", motionOnly)
	}
	if combined.motion != 0 || combined.wheel == 0 || combined.wheel > 8 {
		t.Fatalf("interleaved wheel coordinates did not suppress obsolete motion: %+v", combined)
	}
	if combined.compositions > 16 {
		t.Fatalf("coalesced pointer burst caused composition storm: %+v", combined)
	}
	if combined.queueDrain > 100*time.Millisecond {
		t.Fatalf("obsolete pointer reports remained queued after cadence flush: %+v", combined)
	}
}

func BenchmarkDeepScrollDistinctMotion(b *testing.B) {
	root := pointerBenchRoot(b, 1000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		x := 10 + i%60
		y := 20 + (i/60)%20
		_, _ = root.Update(tea.MouseMotionMsg{X: x, Y: y})
		_ = root.View()
	}
	b.StopTimer()
	root.animationRuntime.Stop()
}
