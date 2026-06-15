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
