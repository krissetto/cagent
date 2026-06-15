package message

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/styles"
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

func TestRenderSubAgentUsesPlainTeamAgentNameColor(t *testing.T) {
	m := New(types.SubAgent(types.SubAgentInfo{Kind: types.SubAgentEventTurnCompleted, AgentName: "director", ShortID: "338dd"}), nil)
	out := m.Render(80)
	plain := ansi.Strip(out)

	if !strings.Contains(out, styles.AgentAccentStyleFor("director").Render("director")) {
		t.Fatalf("expected subagent line to render agent with team accent style, got: %q", out)
	}
	if strings.Contains(plain, "  director  ") {
		t.Fatalf("agent name should be plain colored text, not a padded badge: %q", plain)
	}
}

func TestRenderSubAgentFailureShowsDetail(t *testing.T) {
	m := New(types.SubAgent(types.SubAgentInfo{Kind: types.SubAgentEventFailed, AgentName: "reviewer", ShortID: "def34", Detail: "boom", Truncated: true}), nil)
	out := m.Render(80)

	if !strings.Contains(out, "failed") || !strings.Contains(out, "boom") {
		t.Fatalf("expected failure detail, got: %q", out)
	}
}
