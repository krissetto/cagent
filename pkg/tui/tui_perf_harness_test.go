package tui

import (
	"testing"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/components/spinner"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/service/supervisor"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func wallClockRoot(tb testing.TB, width, height int) *appModel {
	tb.Helper()
	if setter, ok := tb.(interface{ Setenv(key, value string) }); ok {
		setter.Setenv("HOME", tb.TempDir())
	}
	sess := &session.Session{ID: "profile", Title: "profile"}
	a := app.New(tb.Context(), stubRuntime{}, sess)
	m := New(tb.Context(), nil, a, "", func() {}, WithHideSidebar()).(*appModel)
	m.supervisor = supervisor.New(nil)
	ss := service.NewSessionState(sess)
	ss.SetCurrentAgentName("root")
	page := chat.New(m.ar, tb.Context(), a, ss, chat.WithHideSidebar())
	_ = page.SetSize(width, height-9)
	m.chatPages = map[string]chat.Page{}
	m.sessionStates = map[string]*service.SessionState{}
	m.supervisor.AddSession(tb.Context(), a, sess, "", nil)
	m.chatPages["profile"], m.sessionStates["profile"] = page, ss
	m.chatPage, m.sessionState, m.application = page, ss, a
	m.workingSpinner = spinner.New(m.ar, spinner.ModeSpinnerOnly, styles.SpinnerDotsHighlightStyle)
	m.handleWindowResize(width, height)
	_ = m.Init() // synchronously loads the session; returned one-shot commands are warm-up only
	_ = m.View()
	return m
}
