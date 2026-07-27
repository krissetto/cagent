package sidebar

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// newAgentPanelSidebar builds a sidebar whose current agent is "root" and whose
// roster is set, ready to render the Agents panel at the given outer width.
// The panel is pinned to the detailed card mode, which most panel tests
// target; compact-roster tests build their own sidebar (see
// agent_info_mode_test.go). The transfer-box animation is always stopped on
// cleanup so tests that start it (via SetAgentSwitching) cannot leak a
// registration on the global animation coordinator into parallel tests.
func newAgentPanelSidebar(t *testing.T, width int, agents ...runtime.AgentDetails) *model {
	t.Helper()
	sess := session.New()
	ss := service.NewSessionState(sess)
	ss.SetCurrentAgentName("root")
	m := New(animation.NewRuntime(), t.Context(), ss).(*model)
	m.sessionHasContent = true
	m.titleGenerated = true
	m.sessionTitle = "Test"
	m.currentAgent = "root"
	m.availableAgents = agents
	m.width = width
	m.height = 200
	m.SetAgentInfoMode(AgentInfoDetailed)
	t.Cleanup(m.transferAnimation.Stop)
	return m
}

// renderAgentPanel returns the ANSI-stripped lines of the Agents panel body.
func renderAgentPanel(m *model) []string {
	out := ansi.Strip(m.agentInfo(m.contentWidth(false)))
	return strings.Split(out, "\n")
}

const tabHeaderLines = 2 // tab title + TabStyle top padding before the body

// agentBody returns the ANSI-stripped panel body lines aligned 1:1 with
// m.agentLineOwners (populated as a side effect of rendering).
func agentBody(m *model) (body []string) {
	lines := renderAgentPanel(m)
	return lines[tabHeaderLines : tabHeaderLines+len(m.agentLineOwners)]
}

// agentCard returns all ANSI-stripped content lines owned by the named agent:
// the name line, the model line, and its labeled metric line(s).
func agentCard(m *model, name string) []string {
	body := agentBody(m)
	var lines []string
	for j, owner := range m.agentLineOwners {
		if owner == name {
			lines = append(lines, body[j])
		}
	}
	return lines
}

// agentLines returns the first two ANSI-stripped content lines owned by the
// named agent: line1 (name + shortcut) and line2 (provider/model).
func agentLines(m *model, name string) (line1, line2 string) {
	card := agentCard(m, name)
	switch len(card) {
	case 0:
		return "", ""
	case 1:
		return card[0], ""
	}
	return card[0], card[1]
}

// agentMetrics returns the joined ANSI-stripped metric line(s) of the named
// agent's card (everything after the name and model lines).
func agentMetrics(m *model, name string) string {
	card := agentCard(m, name)
	if len(card) <= 2 {
		return ""
	}
	return strings.Join(card[2:], "\n")
}

func TestClassifyThinking(t *testing.T) {
	t.Parallel()

	cases := []struct {
		label    string
		wantKind thinkingKind
		wantTok  int64
	}{
		{"", thinkingNone, 0},
		{"off", thinkingOff, 0},
		{"adaptive", thinkingAdaptive, 0},
		{"8192", thinkingTokens, 8192},
		{"high", thinkingLevel, 0},
		{"minimal", thinkingLevel, 0},
	}
	for _, c := range cases {
		kind, tok := classifyThinking(c.label)
		assert.Equalf(t, c.wantKind, kind, "kind for %q", c.label)
		assert.Equalf(t, c.wantTok, tok, "tokens for %q", c.label)
	}
}

// TestAgentEntryLayout verifies an agent renders as a labeled mini-card:
// line 1 carries the name and "^N" shortcut (no description), line 2 the
// provider/model, and the metric lines the labeled effort gauge, context and
// cost.
func TestAgentEntryLayout(t *testing.T) {
	t.Parallel()

	m := newAgentPanelSidebar(t, 40,
		runtime.AgentDetails{Name: "root", Provider: "anthropic", Model: "claude-opus-4-8", Description: "Executive assistant", Thinking: "high"},
	)

	line1, line2 := agentLines(m, "root")
	require.NotEmpty(t, line1)
	assert.Contains(t, line1, "root", "line 1 shows the agent name")
	assert.Contains(t, line1, "^1", "line 1 shows the switch shortcut")
	assert.NotContains(t, line1, "Executive assistant", "description is not shown")
	assert.Contains(t, line2, "anthropic/claude-opus-4-8", "line 2 shows the provider/model")

	metrics := agentMetrics(m, "root")
	assert.Contains(t, metrics, "Effort "+gaugePattern(4)+" high", "metrics carry the labeled full effort gauge with its value")
	assert.Contains(t, metrics, "Context —", "metrics carry the labeled context (unknown before any run)")
	assert.Contains(t, metrics, "Cost —", "metrics carry the labeled cost (unknown before any run)")

	body := strings.Join(agentBody(m), "\n")
	assert.NotContains(t, body, "Executive assistant", "description is not shown anywhere")
}

// TestCurrentAgentMarker verifies the current agent is marked with ▶ while the
// other agents are not, and that each agent owns exactly its card's content
// lines (separators are unowned).
func TestCurrentAgentMarker(t *testing.T) {
	t.Parallel()

	m := newAgentPanelSidebar(t, 40,
		runtime.AgentDetails{Name: "first", Provider: "openai", Model: "gpt-5.4-mini", Thinking: "off"},
		runtime.AgentDetails{Name: "root", Provider: "anthropic", Model: "claude-opus-4-8", Thinking: "high"},
		runtime.AgentDetails{Name: "last", Provider: "google", Model: "gemini-flash", Thinking: "off"},
	)

	rootLine1, _ := agentLines(m, "root")
	firstLine1, _ := agentLines(m, "first")
	require.NotEmpty(t, rootLine1)
	require.NotEmpty(t, firstLine1)
	assert.Contains(t, rootLine1, "▶", "current agent is marked with ▶")
	assert.NotContains(t, firstLine1, "▶", "non-current agents have no marker")

	// Each agent owns exactly its card's content lines; separators are unowned.
	counts := map[string]int{}
	blanks := 0
	for _, owner := range m.agentLineOwners {
		if owner == "" {
			blanks++
			continue
		}
		counts[owner]++
	}
	for _, name := range []string{"first", "root", "last"} {
		assert.Equalf(t, len(agentCard(m, name)), counts[name], "agent %q owns exactly its card lines", name)
		assert.GreaterOrEqualf(t, counts[name], 3, "agent %q renders at least name, model and one metric line", name)
	}
	assert.Positive(t, blanks, "entries are separated by blank, unowned lines")
}

// TestShortcutAtRightmost verifies the "^N" shortcut is the last visible content
// on the name line: nothing is rendered to the right of it.
func TestShortcutAtRightmost(t *testing.T) {
	t.Parallel()

	m := newAgentPanelSidebar(t, 40,
		runtime.AgentDetails{Name: "root", Provider: "anthropic", Model: "opus", Thinking: "high"},
		runtime.AgentDetails{Name: "alpha", Provider: "openai", Model: "gpt-5.4-mini", Thinking: "8192"},
	)

	for _, name := range []string{"root", "alpha"} {
		line1, _ := agentLines(m, name)
		line := strings.TrimRight(line1, " ")
		require.NotEmpty(t, line)
		assert.Truef(t, strings.HasSuffix(line, "^1") || strings.HasSuffix(line, "^2"),
			"line for %q must end with its shortcut, got %q", name, line)
	}
}

// TestShortcutColumnAlignment verifies the shortcuts align at a single right
// column across name lines regardless of name length or badge width.
func TestShortcutColumnAlignment(t *testing.T) {
	t.Parallel()

	m := newAgentPanelSidebar(t, 40,
		runtime.AgentDetails{Name: "root", Provider: "anthropic", Model: "opus", Thinking: "high"},
		runtime.AgentDetails{Name: "a", Provider: "openai", Model: "gpt-4o", Thinking: "off"},
		runtime.AgentDetails{Name: "longer-name", Provider: "openai", Model: "gpt-5.4", Thinking: "8192"},
	)

	end := -1
	for _, name := range []string{"root", "a", "longer-name"} {
		line1, _ := agentLines(m, name)
		line := strings.TrimRight(line1, " ")
		w := len([]rune(line))
		if end == -1 {
			end = w
		} else {
			assert.Equalf(t, end, w, "shortcuts for %q must end in a single column", name)
		}
	}
}

// TestEffortVocabularyOnCard verifies the effort vocabulary renders on the
// card's labeled metric line: effort levels keep the full six-cell gauge with
// the level word, token budgets keep the token glyph with the budget,
// adaptive reads "auto", and off shows an empty gauge with the word "off".
func TestEffortVocabularyOnCard(t *testing.T) {
	t.Parallel()

	m := newAgentPanelSidebar(t, 40,
		runtime.AgentDetails{Name: "root", Provider: "anthropic", Model: "opus", Thinking: "high"},
		runtime.AgentDetails{Name: "alpha", Provider: "openai", Model: "gpt-5.4-mini", Thinking: "off"},
		runtime.AgentDetails{Name: "beta", Provider: "openai", Model: "gpt-5.4", Thinking: "high"},
		runtime.AgentDetails{Name: "gamma", Provider: "openai", Model: "gpt-4o", Thinking: "8192"},
		runtime.AgentDetails{Name: "delta", Provider: "google", Model: "gemini", Thinking: "adaptive"},
		runtime.AgentDetails{Name: "plain", Provider: "openai", Model: "gpt-4o"},
	)

	want := map[string]string{
		"alpha": "Effort " + gaugePattern(0) + " off",
		"beta":  "Effort " + gaugePattern(4) + " high",
		"gamma": "Effort " + styles.TokenGlyph + " 8.2K",
		"delta": "Effort auto",
	}
	for name, effortText := range want {
		metrics := agentMetrics(m, name)
		require.NotEmptyf(t, metrics, "card for %q should render metrics", name)
		assert.Containsf(t, metrics, effortText, "card %q should show %q", name, effortText)
	}

	assert.NotContains(t, agentMetrics(m, "plain"), "Effort",
		"a model with no thinking configuration has no effort metric")
}

// TestModelLineLeftTruncated verifies the provider/model on line 2 keeps its
// informative tail (left-truncation with a leading ellipsis) when it overflows.
func TestModelLineLeftTruncated(t *testing.T) {
	t.Parallel()

	m := newAgentPanelSidebar(t, 28,
		runtime.AgentDetails{Name: "root", Provider: "anthropic", Model: "opus", Thinking: "high"},
		runtime.AgentDetails{Name: "agent2", Provider: "anthropic", Model: "claude-sonnet-4-6", Thinking: "off"},
	)

	_, line2 := agentLines(m, "agent2")
	require.NotEmpty(t, line2)
	assert.Contains(t, line2, "…", "overflowing model is left-truncated with an ellipsis")
	assert.Contains(t, line2, "-4-6", "informative model tail survives left-truncation")
}

// TestMoreThanNineAgentsNoShortcutBeyond9 verifies agents past the 9th get no
// "^N" shortcut hint.
func TestMoreThanNineAgentsNoShortcutBeyond9(t *testing.T) {
	t.Parallel()

	agents := []runtime.AgentDetails{
		{Name: "root", Provider: "anthropic", Model: "opus", Thinking: "high"},
	}
	for i := 2; i <= 12; i++ {
		agents = append(agents, runtime.AgentDetails{
			Name:     "agent" + string(rune('a'+i)),
			Provider: "openai",
			Model:    "gpt-4o",
			Thinking: "off",
		})
	}
	m := newAgentPanelSidebar(t, 40, agents...)

	body := strings.Join(renderAgentPanel(m), "\n")
	assert.Contains(t, body, "^9", "the 9th agent keeps its shortcut")
	assert.NotContains(t, body, "^10", "agents beyond the 9th have no shortcut")
}

// TestNarrowWidthKeepsFullGauge verifies that at a narrow width the effort
// gauge keeps its full six cells on a dedicated metric line (dropping only
// the value word when it does not fit), the context label compacts to "Ctx",
// and the model still occupies line 2.
func TestNarrowWidthKeepsFullGauge(t *testing.T) {
	t.Parallel()

	m := newAgentPanelSidebar(t, 21,
		runtime.AgentDetails{Name: "root", Provider: "anthropic", Model: "opus", Thinking: "high"},
		runtime.AgentDetails{Name: "agent2", Provider: "anthropic", Model: "claude-sonnet-4-6", Thinking: "high"},
	)

	_, line2 := agentLines(m, "agent2")
	metrics := agentMetrics(m, "agent2")
	assert.Contains(t, metrics, gaugePattern(4), "narrow layout keeps the full six-cell gauge")
	assert.Contains(t, metrics, "Ctx ", "narrow layout compacts the context label")
	assert.NotContains(t, metrics, "Context", "narrow layout does not use the full context label")
	assert.Contains(t, line2, "…", "narrow layout left-truncates the model on line 2")

	contentWidth := m.contentWidth(false)
	for _, line := range agentCard(m, "agent2") {
		assert.LessOrEqual(t, len([]rune(line)), contentWidth, "no card line exceeds the content width: %q", line)
	}
}

// TestClickZonesEveryLine verifies that clicking any rendered agent line (either
// the name line or the model line) resolves to the correct agent.
func TestClickZonesEveryLine(t *testing.T) {
	t.Parallel()

	sess := session.New()
	ss := service.NewSessionState(sess)
	ss.SetCurrentAgentName("root")
	sb := New(animation.NewRuntime(), t.Context(), ss)
	m := sb.(*model)
	m.sessionHasContent = true
	m.titleGenerated = true
	m.sessionTitle = "Test"
	m.currentAgent = "root"
	m.availableAgents = []runtime.AgentDetails{
		{Name: "first", Provider: "openai", Model: "gpt-5.4-mini", Thinking: "off"},
		{Name: "root", Provider: "anthropic", Model: "claude-opus-4-8", Thinking: "high"},
	}
	m.width = 40
	m.height = 200

	_ = sb.View()

	paddingLeft := m.layoutCfg.PaddingLeft
	foundCurrent := false
	foundOther := false
	for y := range len(m.cachedLines) {
		result, name := sb.HandleClickType(paddingLeft+2, y)
		if result != ClickAgent {
			continue
		}
		if name == "root" {
			foundCurrent = true
		}
		if name == "first" {
			foundOther = true
		}
	}
	assert.True(t, foundCurrent, "clicking the current agent's line switches to it")
	assert.True(t, foundOther, "clicking another agent's line switches to it")
}

// TestRosterSeparatesAgentsWithBlankLine verifies a blank separator line is
// inserted between agent cards and that the separator carries an empty owner,
// so each agent owns exactly its card's content lines and click zones stay
// aligned.
func TestRosterSeparatesAgentsWithBlankLine(t *testing.T) {
	t.Parallel()

	m := newAgentPanelSidebar(t, 40,
		runtime.AgentDetails{Name: "root", Provider: "anthropic", Model: "opus", Thinking: "high"},
		runtime.AgentDetails{Name: "alpha", Provider: "openai", Model: "gpt-5.4-mini", Thinking: "off"},
		runtime.AgentDetails{Name: "beta", Provider: "openai", Model: "gpt-5.4", Thinking: "high"},
	)

	_ = renderAgentPanel(m) // populates agentLineOwners

	counts := map[string]int{}
	blanks := 0
	for _, owner := range m.agentLineOwners {
		if owner == "" {
			blanks++
			continue
		}
		counts[owner]++
	}
	for _, name := range []string{"root", "alpha", "beta"} {
		assert.Equalf(t, len(agentCard(m, name)), counts[name],
			"agent %q owns exactly its card lines, not the separator", name)
	}
	assert.Positive(t, blanks, "agents are separated by blank, unowned lines")

	// The panel does not start with a separator, and a blank separator precedes
	// the alpha entry.
	require.NotEmpty(t, m.agentLineOwners)
	assert.NotEmpty(t, m.agentLineOwners[0], "the panel does not start with a separator")
	alphaStart := -1
	for i, owner := range m.agentLineOwners {
		if owner == "alpha" {
			alphaStart = i
			break
		}
	}
	require.Positive(t, alphaStart, "alpha should own lines after root")
	assert.Empty(t, m.agentLineOwners[alphaStart-1], "a blank separator precedes the alpha entry")
}
