package message

import (
	"strings"
	"testing"

	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestRenderSubAgentLifecycleCard(t *testing.T) {
	m := New(types.SubAgent(types.SubAgentInfo{Kind: types.SubAgentEventTurnCompleted, AgentName: "greppy", ShortID: "abc12"}), nil)
	out := m.Render(80)

	if !strings.Contains(out, "greppy") || !strings.Contains(out, "abc12") || !strings.Contains(out, "turn finished") {
		t.Fatalf("unexpected subagent render: %q", out)
	}
	if strings.Contains(out, "Preview") {
		t.Fatalf("turn-finished card should stay compact, got: %q", out)
	}
	if got := m.SubAgentShortRef(); got != "abc12" {
		t.Fatalf("SubAgentShortRef() = %q", got)
	}
}

func TestRenderSubAgentFailureShowsDetail(t *testing.T) {
	m := New(types.SubAgent(types.SubAgentInfo{Kind: types.SubAgentEventFailed, AgentName: "reviewer", ShortID: "def34", Detail: "boom", Truncated: true}), nil)
	out := m.Render(80)

	if !strings.Contains(out, "failed") || !strings.Contains(out, "boom") {
		t.Fatalf("expected failure detail, got: %q", out)
	}
}
