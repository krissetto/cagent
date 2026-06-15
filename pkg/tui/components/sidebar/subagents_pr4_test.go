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
	m.SetLiveSessionTree(&runtime.LiveSessionTree{Root: &runtime.LiveSessionNode{
		ID: "root",
		Children: []*runtime.LiveSessionNode{
			{ID: "director-a-000000", AgentName: "director-a", Status: "waiting", Live: true, Children: []*runtime.LiveSessionNode{
				{ID: "worker-a-000000", AgentName: "worker-a", Status: "waiting", Live: true},
			}},
			{ID: "director-b-000000", AgentName: "director-b", Status: "waiting", Live: true, Children: []*runtime.LiveSessionNode{
				{ID: "worker-b-000000", AgentName: "worker-b", Status: "waiting", Live: true},
			}},
		},
	}})

	plainLines := stripANSILines(strings.Split(m.subagentsSection(90), "\n"))
	directorA := findLineContaining(t, plainLines, "director-a")
	workerA := findLineContaining(t, plainLines, "worker-a")
	directorB := findLineContaining(t, plainLines, "director-b")
	workerB := findLineContaining(t, plainLines, "worker-b")
	if !strings.Contains(directorA, "├ • director-a") {
		t.Fatalf("first direct child should have tree branch and stable dot: %q", directorA)
	}
	if !strings.Contains(workerA, "│ └ • worker-a") {
		t.Fatalf("nested child should retain ancestor guide: %q", workerA)
	}
	if !strings.Contains(directorB, "└ • director-b") {
		t.Fatalf("last direct child should have closing branch: %q", directorB)
	}
	if !strings.Contains(workerB, "  └ • worker-b") {
		t.Fatalf("nested child under last direct child should avoid stray guide: %q", workerB)
	}
}
