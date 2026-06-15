package tui

import (
	"testing"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/service/supervisor"
)

func TestRoutedRootTreeEventUpdatesActiveChildSidebarImmediately(t *testing.T) {
	rootPage := &mockChatPage{}
	childPage := &mockChatPage{}
	m := &appModel{
		supervisor:    supervisor.New(nil),
		chatPages:     map[string]chat.Page{"root": rootPage, "child": childPage},
		sessionStates: map[string]*service.SessionState{"root": {}, "child": {}},
		chatPage:      childPage,
	}
	m.supervisor.AddSession(t.Context(), nil, testSession("root"), "", nil)
	m.supervisor.AddSession(t.Context(), nil, testSession("child"), "", nil)
	m.supervisor.SwitchTo("child")

	treeEvent := &runtime.LiveSessionTreeChangedEvent{Tree: &runtime.LiveSessionTree{Root: &runtime.LiveSessionNode{
		ID:       "root",
		Children: []*runtime.LiveSessionNode{{ID: "child", AgentName: "greppy", Status: "running", Live: true}},
	}}}
	_, cmd := m.handleRoutedMsg(messages.RoutedMsg{SessionID: "root", Inner: treeEvent})
	if cmd != nil {
		_ = cmd()
	}

	if got := len(rootPage.updates); got != 1 {
		t.Fatalf("expected root page to receive routed event once, got %d", got)
	}
	if got := len(childPage.updates); got != 0 {
		t.Fatalf("expected active child transcript not to receive root event, got %d", got)
	}
	if got := len(childPage.sidebarEvents); got != 1 {
		t.Fatalf("expected active child sidebar to receive related root tree event immediately, got %d", got)
	}
}

func testSession(id string) *session.Session { return &session.Session{ID: id} }

func TestRoutedRootSubagentStatusAndTitleEventsUpdateActiveChildSidebarImmediately(t *testing.T) {
	rootPage := &mockChatPage{}
	childPage := &mockChatPage{}
	m := &appModel{
		supervisor:    supervisor.New(nil),
		chatPages:     map[string]chat.Page{"root": rootPage, "child": childPage},
		sessionStates: map[string]*service.SessionState{"root": {}, "child": {}},
		chatPage:      childPage,
	}
	m.supervisor.AddSession(t.Context(), nil, testSession("root"), "", nil)
	m.supervisor.AddSession(t.Context(), nil, testSession("child"), "", nil)
	m.supervisor.SwitchTo("child")

	statusEvent := &runtime.SubAgentUpdateEvent{Envelope: runtime.SubagentEnvelope{ParentSessionID: "root", SubAgentID: "child", AgentName: "greppy", Status: "waiting"}}
	_, cmd := m.handleRoutedMsg(messages.RoutedMsg{SessionID: "root", Inner: statusEvent})
	if cmd != nil {
		_ = cmd()
	}
	titleEvent := runtime.SessionTitle("child", "Finished scout")
	_, cmd = m.handleRoutedMsg(messages.RoutedMsg{SessionID: "root", Inner: titleEvent})
	if cmd != nil {
		_ = cmd()
	}

	if got := len(rootPage.updates); got != 2 {
		t.Fatalf("expected hidden root page to receive both routed events, got %d", got)
	}
	if got := len(childPage.updates); got != 0 {
		t.Fatalf("expected active child transcript not to receive root-routed events, got %d", got)
	}
	if got := len(childPage.sidebarEvents); got != 2 {
		t.Fatalf("expected active child sidebar to receive status and title immediately, got %d", got)
	}
	if got := m.sessionStates["child"].SessionTitle(); got != "Finished scout" {
		t.Fatalf("expected active child session title updated from routed child title, got %q", got)
	}
	if runner := m.supervisor.GetRunner("child"); runner == nil || runner.Title != "Finished scout" {
		t.Fatalf("expected active child runner title updated from routed child title, got %#v", runner)
	}
}
