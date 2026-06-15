package sidebar

import (
	"strings"
	"testing"
	"time"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func TestSubagentsSectionRowsAreCompactAndNested(t *testing.T) {
	m := New(&service.SessionState{}).(*model)
	m.SetLiveSessionTree(&runtime.LiveSessionTree{Root: &runtime.LiveSessionNode{
		ID: "root",
		Children: []*runtime.LiveSessionNode{{
			ID: "child12345", AgentName: "greppy", Status: "running", Live: true, Preview: "prompt clutter", LastPreview: "response clutter",
			Children: []*runtime.LiveSessionNode{{ID: "nested12345", AgentName: "reviewer", Status: "waiting", Live: true, LastPreview: "nested clutter"}},
		}},
	}})
	out := m.subagentsSection(60)

	for _, want := range []string{"Subagents", "greppy", "child", "working", "reviewer", "neste", "idle"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected %q in %q", want, out)
		}
	}
	if strings.Contains(out, "prompt clutter") || strings.Contains(out, "response clutter") || strings.Contains(out, "nested clutter") {
		t.Fatalf("subagent rows should not show prompt/response clutter: %q", out)
	}
}

func TestSubagentStartAndUpdateManageSpinnerState(t *testing.T) {
	m := New(&service.SessionState{}).(*model)
	startedAt := time.Now()
	updated, cmd := m.Update(&runtime.SubAgentStartedEvent{SubAgent: runtime.SubagentInfo{ID: "child12345", ShortID: "child", AgentName: "greppy", State: "running", CreatedAt: startedAt}})
	m = updated.(*model)
	if cmd == nil {
		t.Fatalf("expected spinner init command")
	}
	if _, ok := m.subagentSpinners["child12345"]; !ok {
		t.Fatalf("expected spinner for started subagent")
	}

	updated, cmd = m.Update(&runtime.SubAgentUpdateEvent{Envelope: runtime.SubagentEnvelope{SubAgentID: "child12345", AgentName: "greppy", Status: "waiting", Kind: "turn_completed"}})
	m = updated.(*model)
	if _, ok := m.subagentSpinners["child12345"]; ok {
		t.Fatalf("expected idle update to stop spinner")
	}
	if cmd != nil {
		_ = cmd()
	}
}

func TestSubagentRowsUseSessionTitleWhenPresent(t *testing.T) {
	m := New(&service.SessionState{}).(*model)
	m.SetLiveSessionTree(&runtime.LiveSessionTree{Root: &runtime.LiveSessionNode{
		ID:       "root",
		Children: []*runtime.LiveSessionNode{{ID: "child12345", AgentName: "greppy", Title: "Research API", Status: "waiting", Live: true, CreatedAt: time.Now()}},
	}})

	plain := strings.Join(stripANSILines(strings.Split(m.subagentsSection(80), "\n")), "\n")
	if !strings.Contains(plain, "Research API") {
		t.Fatalf("expected titled child row, got %q", plain)
	}
	if strings.Contains(plain, "• greppy") {
		t.Fatalf("expected child title to replace agent label, got %q", plain)
	}
}

func TestSubagentTitleEventUpdatesLiveTreeNode(t *testing.T) {
	m := New(&service.SessionState{}).(*model)
	m.SetLiveSessionTree(&runtime.LiveSessionTree{Root: &runtime.LiveSessionNode{
		ID:       "root",
		Children: []*runtime.LiveSessionNode{{ID: "child12345", AgentName: "greppy", Status: "waiting", Live: true, CreatedAt: time.Now()}},
	}})

	updated, _ := m.Update(runtime.SessionTitle("child12345", "Finished scout"))
	m = updated.(*model)

	plain := strings.Join(stripANSILines(strings.Split(m.subagentsSection(80), "\n")), "\n")
	if !strings.Contains(plain, "Finished scout") {
		t.Fatalf("expected child title event to update subagent row, got %q", plain)
	}
}

func TestSubagentRowsUseStableStatusPrefix(t *testing.T) {
	m := New(&service.SessionState{}).(*model)
	created := time.Now().Add(-2 * time.Minute)
	m.SetLiveSessionTree(&runtime.LiveSessionTree{Root: &runtime.LiveSessionNode{
		ID: "root",
		Children: []*runtime.LiveSessionNode{
			{ID: "running12345", AgentName: "greppy", Status: "running", Live: true, CreatedAt: created},
			{ID: "waiting12345", AgentName: "reviewer", Status: "waiting", Live: true, CreatedAt: created},
		},
	}})
	out := m.subagentsSection(80)
	plain := stripANSILines(strings.Split(out, "\n"))
	running := findLineContaining(t, plain, "greppy")
	waiting := findLineContaining(t, plain, "reviewer")

	if strings.Index(running, "greppy") != strings.Index(waiting, "reviewer") {
		t.Fatalf("agent names should start in stable column with or without spinner:\nrunning=%q\nwaiting=%q", running, waiting)
	}
	if !strings.Contains(waiting, "• reviewer") {
		t.Fatalf("idle row should reserve prefix indicator column: %q", waiting)
	}
}

func TestSubagentHoverShowsRelativeCreatedAt(t *testing.T) {
	m := New(&service.SessionState{}).(*model)
	m.SetLiveSessionTree(&runtime.LiveSessionTree{Root: &runtime.LiveSessionNode{
		ID:       "root",
		Children: []*runtime.LiveSessionNode{{ID: "child12345", AgentName: "greppy", Status: "waiting", Live: true, CreatedAt: time.Now().Add(-2 * time.Minute)}},
	}})

	plain := strings.Join(stripANSILines(strings.Split(m.subagentsSection(80), "\n")), "\n")
	if strings.Contains(plain, "2m ago") {
		t.Fatalf("relative time should only show on hover: %q", plain)
	}
	m.hoveredSubagentID = "child12345"
	plain = strings.Join(stripANSILines(strings.Split(m.subagentsSection(80), "\n")), "\n")
	if !strings.Contains(plain, "2m ago") {
		t.Fatalf("hovered row should show relative created-at: %q", plain)
	}
	if strings.Contains(plain, "idle") {
		t.Fatalf("hovered row should replace status with relative time: %q", plain)
	}
}

func TestSubagentTreeRendersDirectChildrenAndNestedDescendants(t *testing.T) {
	m := New(&service.SessionState{}).(*model)
	now := time.Now()
	m.SetLiveSessionTree(&runtime.LiveSessionTree{Root: &runtime.LiveSessionNode{
		ID: "root",
		Children: []*runtime.LiveSessionNode{
			{ID: "director-a-000000", AgentName: "director-a", Status: "waiting", Live: true, CreatedAt: now.Add(-2 * time.Minute), Children: []*runtime.LiveSessionNode{
				{ID: "worker-a-000000", AgentName: "worker-a", Status: "waiting", Live: true, CreatedAt: now.Add(-3 * time.Minute)},
			}},
			{ID: "director-b-000000", AgentName: "director-b", Status: "waiting", Live: true, CreatedAt: now.Add(-1 * time.Minute), Children: []*runtime.LiveSessionNode{
				{ID: "worker-b-000000", AgentName: "worker-b", Status: "waiting", Live: true, CreatedAt: now.Add(-4 * time.Minute)},
			}},
		},
	}})

	plainLines := stripANSILines(strings.Split(m.subagentsSection(90), "\n"))
	directorA := findLineContaining(t, plainLines, "director-a")
	workerA := findLineContaining(t, plainLines, "worker-a")
	directorB := findLineContaining(t, plainLines, "director-b")
	workerB := findLineContaining(t, plainLines, "worker-b")
	if strings.Contains(directorA, "├") || strings.Contains(directorA, "└") || strings.Contains(directorA, "│") {
		t.Fatalf("direct child should not have tree branch glyphs: %q", directorA)
	}
	if strings.Contains(directorB, "├") || strings.Contains(directorB, "└") || strings.Contains(directorB, "│") {
		t.Fatalf("direct child should not have tree branch glyphs: %q", directorB)
	}
	if !strings.Contains(directorA, "• director-a") || !strings.Contains(directorB, "• director-b") {
		t.Fatalf("direct children should keep stable dot prefix:\ndirectorA=%q\ndirectorB=%q", directorA, directorB)
	}
	if !strings.Contains(workerA, "  └ • worker-a") {
		t.Fatalf("nested child should retain tree branch: %q", workerA)
	}
	if !strings.Contains(workerB, "│ └ • worker-b") {
		t.Fatalf("nested child under newer first sibling should retain ancestor guide: %q", workerB)
	}
}

func TestSubagentSiblingsRenderNewestCreatedFirst(t *testing.T) {
	m := New(&service.SessionState{}).(*model)
	now := time.Now()
	m.SetLiveSessionTree(&runtime.LiveSessionTree{Root: &runtime.LiveSessionNode{
		ID: "root",
		Children: []*runtime.LiveSessionNode{
			{ID: "older-000000", AgentName: "older", Status: "waiting", Live: true, CreatedAt: now.Add(-3 * time.Minute)},
			{ID: "newest-000000", AgentName: "newest", Status: "waiting", Live: true, CreatedAt: now.Add(-1 * time.Minute)},
			{ID: "middle-000000", AgentName: "middle", Status: "waiting", Live: true, CreatedAt: now.Add(-2 * time.Minute), Children: []*runtime.LiveSessionNode{
				{ID: "nested-old-000000", AgentName: "nested-old", Status: "waiting", Live: true, CreatedAt: now.Add(-4 * time.Minute)},
				{ID: "nested-new-000000", AgentName: "nested-new", Status: "waiting", Live: true, CreatedAt: now.Add(-30 * time.Second)},
			}},
		},
	}})

	plain := strings.Join(stripANSILines(strings.Split(m.subagentsSection(100), "\n")), "\n")
	assertBefore(t, plain, "newest", "middle")
	assertBefore(t, plain, "middle", "older")
	assertBefore(t, plain, "nested-new", "nested-old")
}

func assertBefore(t *testing.T, haystack, earlier, later string) {
	t.Helper()
	earlierIndex := strings.Index(haystack, earlier)
	laterIndex := strings.Index(haystack, later)
	if earlierIndex == -1 || laterIndex == -1 {
		t.Fatalf("expected both %q and %q in %q", earlier, later, haystack)
	}
	if earlierIndex >= laterIndex {
		t.Fatalf("expected %q before %q in %q", earlier, later, haystack)
	}
}

func TestParentIdleClearsRootAgentSpinnerImmediately(t *testing.T) {
	m := New(&service.SessionState{}).(*model)
	m.SetTeamInfo([]runtime.AgentDetails{{Name: "root", Description: "parent"}})
	m.sessionState.SetCurrentAgentName("root")

	updated, cmd := m.Update(runtime.StreamStarted("root-session", "root"))
	m = updated.(*model)
	if cmd == nil {
		t.Fatalf("expected root stream to start spinner")
	}
	if m.workingAgent != "root" || !m.spinnerActive {
		t.Fatalf("expected root spinner active, workingAgent=%q spinnerActive=%v", m.workingAgent, m.spinnerActive)
	}

	updated, cmd = m.Update(&runtime.ParentIdleEvent{SessionID: "root-session"})
	m = updated.(*model)
	if cmd != nil {
		_ = cmd()
	}
	if m.workingAgent != "" || m.spinnerActive {
		t.Fatalf("expected ParentIdle to clear root spinner immediately, workingAgent=%q spinnerActive=%v", m.workingAgent, m.spinnerActive)
	}
	if out := strings.Join(stripANSILines(strings.Split(m.agentInfo(80), "\n")), "\n"); strings.Contains(out, "⠋") || strings.Contains(out, "⠙") {
		t.Fatalf("root agent row should not render spinner after ParentIdle: %q", out)
	}
}

func TestParentIdleKeepsChildSpinnerInSubagentsSection(t *testing.T) {
	m := New(&service.SessionState{}).(*model)
	m.SetTeamInfo([]runtime.AgentDetails{{Name: "root", Description: "parent"}})
	m.sessionState.SetCurrentAgentName("root")
	m.SetLiveSessionTree(&runtime.LiveSessionTree{Root: &runtime.LiveSessionNode{
		ID:       "root-session",
		Children: []*runtime.LiveSessionNode{{ID: "child-session", AgentName: "greppy", Status: "running", Live: true}},
	}})

	updated, _ := m.Update(runtime.StreamStarted("root-session", "root"))
	m = updated.(*model)
	updated, _ = m.Update(&runtime.ParentIdleEvent{SessionID: "root-session"})
	m = updated.(*model)

	if m.workingAgent != "" || m.spinnerActive {
		t.Fatalf("expected root spinner stopped while waiting, workingAgent=%q spinnerActive=%v", m.workingAgent, m.spinnerActive)
	}
	plain := strings.Join(stripANSILines(strings.Split(m.subagentsSection(80), "\n")), "\n")
	if !strings.Contains(plain, "greppy") || !strings.Contains(plain, "working") {
		t.Fatalf("running child should still be reflected in subagents section: %q", plain)
	}
}
