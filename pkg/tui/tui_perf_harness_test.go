//nolint:unused,unparam // Shared performance harness is activated by descendant benchmark tests.
package tui

import (
	"fmt"
	"os"
	"runtime"
	"runtime/pprof"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/app"
	chatmsg "github.com/docker/docker-agent/pkg/chat"
	agentruntime "github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	messagecomponent "github.com/docker/docker-agent/pkg/tui/components/messages"
	"github.com/docker/docker-agent/pkg/tui/components/spinner"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/service/supervisor"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func requireFullPerfMatrix(t *testing.T) {
	t.Helper()
	if os.Getenv("AP_TUI_FULL_PERF_MATRIX") != "1" {
		t.Skip("set AP_TUI_FULL_PERF_MATRIX=1 for the expensive matrix")
	}
}

type wallClockStopMsg struct{}

type (
	wallClockChunkMsg        struct{ n int }
	wallClockScrollMsg       struct{}
	wallClockCheckpointMsg   struct{ second int }
	wallClockProfileStartMsg struct{}
	wallClockProfileStopMsg  struct{}
)

type countedRootProgram struct {
	root         *appModel
	initCmd      tea.Cmd
	updates      atomic.Uint64
	views        atomic.Uint64
	compositions atomic.Uint64
	phase3       messagecomponent.WorkCounters
	phase10      messagecomponent.WorkCounters
	phase15      messagecomponent.WorkCounters
	profileFile  *os.File
}

func (m *countedRootProgram) Init() tea.Cmd { return m.initCmd }
func (m *countedRootProgram) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	m.updates.Add(1)
	switch msg := msg.(type) {
	case wallClockStopMsg:
		if m.profileFile != nil {
			pprof.StopCPUProfile()
			_ = m.profileFile.Close()
			m.profileFile = nil
		}
		m.root.workingSpinner.Stop()
		m.root.animationRuntime.Stop()
		return m, tea.Quit
	case wallClockChunkMsg:
		content := fmt.Sprintf("chunk-%04d **bold** λ界 ", msg.n)
		if msg.n%8 == 7 {
			content += "\n\n"
		}
		event := agentruntime.AgentChoice("root", "profile", content).(*agentruntime.AgentChoiceEvent)
		updated, cmd := m.root.Update(messages.RoutedMsg{SessionID: "profile", Inner: event})
		m.root = updated.(*appModel)
		return m, tea.Batch(cmd, tea.Tick(time.Second/60, func(time.Time) tea.Msg { return wallClockChunkMsg{n: msg.n + 1} }))
	case wallClockCheckpointMsg:
		if work, ok := m.root.chatPage.(interface {
			WorkCountersForTest() messagecomponent.WorkCounters
		}); ok {
			switch msg.second {
			case 3:
				m.phase3 = work.WorkCountersForTest()
			case 10:
				m.phase10 = work.WorkCountersForTest()
			case 15:
				m.phase15 = work.WorkCountersForTest()
			}
		}
		return m, nil
	case wallClockProfileStartMsg:
		if path := os.Getenv("AP_TUI_CPU_PROFILE"); path != "" {
			f, err := os.Create(path)
			if err != nil {
				panic(err)
			}
			if err := pprof.StartCPUProfile(f); err != nil {
				_ = f.Close()
				panic(err)
			}
			m.profileFile = f
		}
		return m, nil
	case wallClockProfileStopMsg:
		if m.profileFile != nil {
			pprof.StopCPUProfile()
			_ = m.profileFile.Close()
			m.profileFile = nil
		}
		return m, nil
	case wallClockScrollMsg:
		updated, cmd := m.root.Update(tea.KeyPressMsg{Code: 'g'})
		m.root = updated.(*appModel)
		return m, cmd
	}
	updated, cmd := m.root.Update(msg)
	m.root = updated.(*appModel)
	return m, cmd
}

func (m *countedRootProgram) View() tea.View {
	m.views.Add(1)
	if !m.root.viewCacheValid {
		m.compositions.Add(1)
	}
	return m.root.View()
}

type wallClockCountingWriter struct{ writes, bytes atomic.Uint64 }

func (w *wallClockCountingWriter) Write(p []byte) (int, error) {
	w.writes.Add(1)
	w.bytes.Add(uint64(len(p)))
	return len(p), nil
}

func mixedHistorySession(count int) (*session.Session, int, int) {
	body := strings.Repeat("word ", 996) + "**bold** `code` λ界 end" // exactly 1,000 words
	items := make([]session.Item, 0, count)
	totalBytes := 0
	for i := range count {
		id := fmt.Sprintf("call-%04d", i)
		var msg *session.Message
		switch i % 5 {
		case 0:
			msg = session.UserMessage(body)
		case 1:
			msg = &session.Message{AgentName: "root", Message: chatmsg.Message{Role: chatmsg.MessageRoleAssistant, Content: "## Assistant\n\n" + body}}
		case 2:
			msg = &session.Message{AgentName: "root", Message: chatmsg.Message{Role: chatmsg.MessageRoleAssistant, Content: body, ReasoningContent: body}}
		case 3:
			msg = &session.Message{AgentName: "root", Message: chatmsg.Message{Role: chatmsg.MessageRoleAssistant, Content: body, ReasoningContent: body, ToolCalls: []tools.ToolCall{{ID: id, Function: tools.FunctionCall{Name: "read_file", Arguments: `{"path":"fixture"}`}}}, ToolDefinitions: []tools.Tool{{Name: "read_file", Description: body}}}}
		default:
			msg = &session.Message{AgentName: "root", Message: chatmsg.Message{Role: chatmsg.MessageRoleTool, ToolCallID: fmt.Sprintf("call-%04d", i-1), Content: body}}
		}
		items = append(items, session.NewMessageItem(msg))
		totalBytes += len(msg.Message.Content) + len(msg.Message.ReasoningContent)
	}
	return &session.Session{ID: "profile", Title: "profile", Messages: items}, count * 1000, totalBytes
}

func millionWordSession() *session.Session {
	sess, _, _ := mixedHistorySession(1000)
	return sess
}

func wallClockRoot(tb testing.TB, width, height int) (*appModel, time.Duration, runtime.MemStats) {
	tb.Helper()
	if setter, ok := tb.(interface{ Setenv(key, value string) }); ok {
		setter.Setenv("HOME", tb.TempDir())
	}
	started := time.Now()
	sess := &session.Session{ID: "profile", Title: "profile"}
	if os.Getenv("AP_TUI_WALLCLOCK_HUGE") == "1" {
		sess = millionWordSession()
	}
	a := app.New(tb.Context(), stubRuntime{}, sess)
	m := New(tb.Context(), nil, a, "", func() {}, WithHideSidebar()).(*appModel)
	m.supervisor = supervisor.New(nil)
	ss := service.NewSessionState(sess)
	ss.SetCurrentAgentName("root")
	page := chat.New(m.animationRuntime, tb.Context(), a, ss, chat.WithHideSidebar())
	_ = page.SetSize(width, height-9)
	m.chatPages = map[string]chat.Page{}
	m.sessionStates = map[string]*service.SessionState{}
	m.supervisor.AddSession(tb.Context(), a, sess, "", nil)
	m.chatPages["profile"], m.sessionStates["profile"] = page, ss
	m.chatPage, m.sessionState, m.application = page, ss, a
	m.workingSpinner = spinner.New(m.animationRuntime, spinner.ModeSpinnerOnly, styles.SpinnerDotsHighlightStyle)
	m.handleWindowResize(width, height)
	_ = m.Init() // synchronously loads the session; returned one-shot commands are warm-up only
	_ = m.View()
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	return m, time.Since(started), memory
}

// TestWallClockRootProfile is an opt-in real Bubble Tea renderer workload.
// AP_TUI_WALLCLOCK_PROFILE selects idle, spinner, or progressive; the default
// duration is two seconds and AP_TUI_WALLCLOCK_SECONDS may override it.
