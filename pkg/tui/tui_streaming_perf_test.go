package tui

import (
	"fmt"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/app"
	agentruntime "github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/components/spinner"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/service/supervisor"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func streamingBenchmarkRoot(b *testing.B, scrolled bool) *appModel {
	b.Helper()
	m, _, _ := wallClockRoot(b, 120, 40)
	m.supervisor = supervisor.New(nil)
	m.chatPages = map[string]chat.Page{}
	m.sessionStates = map[string]*service.SessionState{}
	sess := &session.Session{ID: "stream", Title: "stream"}
	a := app.New(b.Context(), stubRuntime{}, sess)
	ss := service.NewSessionState(sess)
	ss.SetCurrentAgentName("root")
	page := chat.New(m.animationRuntime, b.Context(), a, ss, chat.WithHideSidebar())
	_ = page.SetSize(120, 31)
	m.supervisor.AddSession(b.Context(), a, sess, "", nil)
	m.chatPages["stream"], m.sessionStates["stream"] = page, ss
	m.chatPage, m.sessionState, m.application = page, ss, a
	m.workingSpinner = spinner.New(m.animationRuntime, spinner.ModeSpinnerOnly, styles.SpinnerDotsHighlightStyle)

	_, _ = m.Update(messages.RoutedMsg{SessionID: "stream", Inner: agentruntime.StreamStarted("stream", "root")})
	prefix := "## Streaming benchmark\n\n"
	for i := range 240 {
		chunk := "Paragraph with **markdown**, `code`, Unicode λ界, and a [link](https://example.com).\n\n"
		if i%12 == 0 {
			chunk = "```go\nfmt.Println(\"stream\")\n```\n\n"
		}
		_, _ = m.Update(messages.RoutedMsg{SessionID: "stream", Inner: agentruntime.AgentChoice("root", "stream", prefix+chunk)})
		prefix = ""
	}
	_ = m.View()
	if scrolled {
		_ = page.FocusMessages()
		_, _ = page.Update(tea.KeyPressMsg{Code: 'g'})
		_ = m.View()
	}
	return m
}

// BenchmarkActualRootStreaming measures Bubble Tea root Update+View at a fixed
// terminal size with the real chat/messages/ScrollView/fade path. The history
// is intentionally much taller than the viewport.
func BenchmarkActualRootStreaming(b *testing.B) {
	for _, paragraphs := range []int{10, 25, 50, 100, 200} {
		b.Run(fmt.Sprintf("follow-tail/%d", paragraphs), func(b *testing.B) {
			m := streamingBenchmarkRoot(b, false)
			chunk := "Paragraph with **markdown**, `code`, Unicode λ界, and a [link](https://example.com).\n\n"
			for range paragraphs {
				_, _ = m.Update(messages.RoutedMsg{SessionID: "stream", Inner: agentruntime.AgentChoice("root", "stream", chunk)})
				_ = m.View()
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				_, _ = m.Update(messages.RoutedMsg{SessionID: "stream", Inner: agentruntime.AgentChoice("root", "stream", chunk)})
				if m.View().Content == "" {
					b.Fatal("root frame is empty")
				}
			}
			b.ReportMetric(1, "root-views/chunk")
		})
	}
}
