package sidebar

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// gaugePattern builds the expected ANSI-stripped gauge string for n filled cells
// of the shared six-cell gauge.
func gaugePattern(filled int) string {
	return strings.Repeat(styles.GaugeFilled, filled) +
		strings.Repeat(styles.GaugeEmpty, toolcommon.EffortGaugeCells-filled)
}

// TestEffortSegmentLevelUsesGauge verifies the level case of effortSegment
// renders the labeled six-cell gauge with the level word (no ✻ glyph), and
// that the minimal degradation keeps the semantic value — the decorative
// gauge goes, never the word.
func TestEffortSegmentLevelUsesGauge(t *testing.T) {
	t.Parallel()

	seg := effortSegment("high", false)
	assert.Equal(t, "Effort "+gaugePattern(4)+" high", ansi.Strip(seg.full),
		"high renders the labeled 4/6-cell gauge with its value")
	assert.NotContains(t, ansi.Strip(seg.full), styles.ThinkingGlyph, "gauge carries no ✻ glyph")
	assert.Equal(t, "Eff high", ansi.Strip(seg.minimal),
		"the minimal form is the compact segment: no gauge, the value stays")
}

// TestEffortSegmentUnknownLevelFallsBackToText verifies an unparseable level
// label keeps a plain labeled text segment (no glyph) so unknown/future labels
// still render.
func TestEffortSegmentUnknownLevelFallsBackToText(t *testing.T) {
	t.Parallel()

	seg := effortSegment("on", false)
	assert.Equal(t, "Effort on", ansi.Strip(seg.full), "unknown label keeps a plain text value")
	assert.NotContains(t, ansi.Strip(seg.full), styles.ThinkingGlyph, "no ✻ glyph")
	assert.Equal(t, "Eff on", ansi.Strip(seg.minimal), "the minimal form keeps the value under the compact label")
}

// TestEffortSegmentVocabulary verifies the full no-✻ segment vocabulary: none
// renders nothing, off is an empty gauge with the word "off", adaptive is
// "auto", a token budget keeps its token glyph with the count, and every
// minimal fallback is the compact form that keeps the semantic value.
func TestEffortSegmentVocabulary(t *testing.T) {
	t.Parallel()

	cases := []struct {
		label   string
		full    string
		minimal string
	}{
		{"", "", ""},
		{"off", "Effort " + gaugePattern(0) + " off", "Eff off"},
		{"adaptive", "Effort auto", "Eff auto"},
		{"8192", "Effort " + styles.TokenGlyph + " 8.2K", "Eff " + styles.TokenGlyph + " 8.2K"},
		{"minimal", "Effort " + gaugePattern(1) + " minimal", "Eff minimal"},
	}
	for _, c := range cases {
		seg := effortSegment(c.label, false)
		assert.Equalf(t, c.full, ansi.Strip(seg.full), "full segment for %q", c.label)
		assert.Equalf(t, c.minimal, ansi.Strip(seg.minimal), "minimal segment for %q", c.label)
		assert.NotContainsf(t, ansi.Strip(seg.full), styles.ThinkingGlyph, "segment for %q must carry no ✻", c.label)
	}
}

// TestEffortSegmentNarrowVocabulary verifies the narrow-card effort
// vocabulary: the label compacts to "Eff", the decorative six-cell gauge is
// omitted while the semantic value stays visible, token budgets keep the
// token glyph with their count, adaptive reads "auto", unknown level words
// keep their text, and nothing degrades: full and minimal are identical.
func TestEffortSegmentNarrowVocabulary(t *testing.T) {
	t.Parallel()

	cases := []struct {
		label string
		want  string
	}{
		{"", ""},
		{"off", "Eff off"},
		{"adaptive", "Eff auto"},
		{"8192", "Eff " + styles.TokenGlyph + " 8.2K"},
		{"high", "Eff high"},
		{"minimal", "Eff minimal"},
		{"on", "Eff on"},
	}
	for _, c := range cases {
		seg := effortSegment(c.label, true)
		assert.Equalf(t, c.want, ansi.Strip(seg.full), "full narrow segment for %q", c.label)
		assert.Equalf(t, c.want, ansi.Strip(seg.minimal),
			"minimal narrow segment for %q — the semantic value never degrades away", c.label)
		assert.NotContainsf(t, ansi.Strip(seg.full), styles.GaugeFilled, "narrow segment for %q carries no gauge", c.label)
		assert.NotContainsf(t, ansi.Strip(seg.full), styles.GaugeEmpty, "narrow segment for %q carries no gauge", c.label)
	}
}

// TestFlowMetricLinesWideEffortFallbackKeepsValue verifies the wide flow
// never trades the semantic effort value for a bare gauge: an effort segment
// that cannot fit a line even on its own — "Effort ▰▱▱▱▱▱ minimal" is 21
// columns — degrades to the compact "Eff minimal" instead of "Effort
// <gauge>", and the freed room lets the next segment share its line.
func TestFlowMetricLinesWideEffortFallbackKeepsValue(t *testing.T) {
	t.Parallel()

	effortSeg := effortSegment("minimal", false)
	require.Equal(t, 21, lipgloss.Width(effortSeg.full), "the full wide segment must overflow the 20-column line")
	ctx := metricSegment{full: "Ctx 6%", minimal: "Ctx 6%"}

	lines := flowMetricLines([]metricSegment{effortSeg, ctx}, 20, false)
	require.Len(t, lines, 1, "the compact fallback frees enough room for the next segment")
	assert.Equal(t, "Eff minimal · Ctx 6%", ansi.Strip(lines[0]),
		"the semantic value survives; the decorative gauge goes")
	assert.LessOrEqual(t, lipgloss.Width(lines[0]), 20, "the fallback line must not overflow")
}

// TestThinkingGaugeValueShowsGaugeAndWord verifies the shared thinking summary
// used by the agent-details dialog is "<gauge> <word>" (no ✻): both the gauge
// and the descriptive word.
func TestThinkingGaugeValueShowsGaugeAndWord(t *testing.T) {
	t.Parallel()

	got := ansi.Strip(toolcommon.ThinkingGaugeValue("high"))
	assert.Equal(t, gaugePattern(4)+" high", got)
	assert.NotContains(t, got, styles.ThinkingGlyph, "summary carries no ✻ glyph")

	// off shows a dim empty gauge plus the word "off".
	gotOff := ansi.Strip(toolcommon.ThinkingGaugeValue("off"))
	assert.Equal(t, strings.Repeat(styles.GaugeEmpty, toolcommon.EffortGaugeCells)+" off", gotOff)

	// Empty label omits the summary entirely.
	assert.Empty(t, ansi.Strip(toolcommon.ThinkingGaugeValue("")))
}

// TestCardGaugeColumnAlignment verifies a roster of effort-level agents renders
// fixed-width six-cell gauges on their labeled effort line, and that the gauges
// all start at the same column (right after the shared "Effort " label).
// Rendered wide enough (56) for the full metric vocabulary.
func TestCardGaugeColumnAlignment(t *testing.T) {
	t.Parallel()

	m := newAgentPanelSidebar(t, 56,
		runtime.AgentDetails{Name: "root", Provider: "anthropic", Model: "opus", Thinking: "high"},
		runtime.AgentDetails{Name: "alpha", Provider: "openai", Model: "gpt-5.4-mini", Thinking: "minimal"},
		runtime.AgentDetails{Name: "beta", Provider: "openai", Model: "gpt-5.4", Thinking: "medium"},
		runtime.AgentDetails{Name: "gamma", Provider: "openai", Model: "gpt-4o", Thinking: "max"},
	)

	wantGauge := map[string]string{
		"alpha": gaugePattern(1),
		"beta":  gaugePattern(3),
		"gamma": gaugePattern(6),
	}
	gaugeStart := -1
	for name, gauge := range wantGauge {
		metrics := agentMetrics(m, name)
		require.NotEmptyf(t, metrics, "card for %q should render metrics", name)
		assert.Containsf(t, metrics, "Effort "+gauge, "card %q should contain labeled gauge %q", name, gauge)

		idx := strings.Index(metrics, gauge)
		require.GreaterOrEqualf(t, idx, 0, "gauge %q must appear in card %q", gauge, metrics)
		start := len([]rune(metrics[:idx]))
		if gaugeStart == -1 {
			gaugeStart = start
		} else {
			assert.Equalf(t, gaugeStart, start, "gauge for %q must start in the shared column", name)
		}
	}
}
