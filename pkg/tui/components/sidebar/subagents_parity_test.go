package sidebar

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestSubagentRowsUseAgentAccentStyle(t *testing.T) {
	t.Parallel()

	m := New(&service.SessionState{}).(*model)
	m.SetLiveSessionTree(&runtime.LiveSessionTree{Root: &runtime.LiveSessionNode{
		ID:       "root",
		Children: []*runtime.LiveSessionNode{{ID: "child12345", AgentName: "greppy", Status: "waiting", Live: true}},
	}})

	out := m.subagentsSection(60)
	require.Contains(t, ansi.Strip(out), "greppy")
	require.NotEqual(t, styles.SubagentAccentStyleFor("greppy").GetForeground(), styles.AgentAccentStyleFor("greppy").GetForeground())
}

func TestSubagentTreeDirectChildrenHaveNoConnectorOrHiddenRootStem(t *testing.T) {
	t.Parallel()

	m := New(&service.SessionState{}).(*model)
	m.SetLiveSessionTree(&runtime.LiveSessionTree{Root: &runtime.LiveSessionNode{
		ID: "root",
		Children: []*runtime.LiveSessionNode{
			{ID: "director-a-000000", AgentName: "director-a", Status: "waiting", Live: true, Children: []*runtime.LiveSessionNode{{ID: "worker-a-000000", AgentName: "worker-a", Status: "waiting", Live: true}}},
			{ID: "director-b-000000", AgentName: "director-b", Status: "waiting", Live: true},
		},
	}})

	plainLines := stripANSILines(strings.Split(m.subagentsSection(80), "\n"))
	directorALine := findLineContaining(t, plainLines, "director-a")
	workerLine := findLineContaining(t, plainLines, "worker-a")
	directorBLine := findLineContaining(t, plainLines, "director-b")
	require.NotContains(t, directorALine, "├")
	require.NotContains(t, directorALine, "└")
	require.NotContains(t, directorBLine, "├")
	require.NotContains(t, directorBLine, "└")
	require.Contains(t, workerLine, "└")
	require.NotContains(t, workerLine, "│ └", "nested row should not carry hidden-root vertical stem")
}

func TestSubagentHoverBrightensAndClears(t *testing.T) {
	t.Parallel()

	sess := session.New()
	m := New(service.NewSessionState(sess)).(*model)
	m.width = 60
	m.height = 30
	m.SetLiveSessionTree(&runtime.LiveSessionTree{Root: &runtime.LiveSessionNode{
		ID:       "root",
		Children: []*runtime.LiveSessionNode{{ID: "child12345", AgentName: "greppy", Status: "waiting", Live: true}},
	}})
	_ = m.View()

	rowY := subagentZoneY(t, m, "child12345")
	updated, _ := m.Update(tea.MouseMotionMsg{X: m.layoutCfg.PaddingLeft + 2, Y: rowY})
	m = updated.(*model)
	require.Equal(t, "child12345", m.hoveredSubagentID)
	require.Contains(t, m.subagentsSection(60), "1;38;2")

	updated, _ = m.Update(tea.MouseMotionMsg{X: m.width + 5, Y: rowY})
	m = updated.(*model)
	require.Empty(t, m.hoveredSubagentID)
}

func TestSubagentClickAndHoverZonesIncludeFailedDetailLine(t *testing.T) {
	t.Parallel()

	m := New(&service.SessionState{}).(*model)
	m.width = 70
	m.height = 30
	m.SetLiveSessionTree(&runtime.LiveSessionTree{Root: &runtime.LiveSessionNode{
		ID:       "root",
		Children: []*runtime.LiveSessionNode{{ID: "failed-child-000", AgentName: "reviewer", Status: "failed", Live: false, Error: "detail line"}},
	}})
	_ = m.View()

	var ys []int
	for y, id := range m.subagentClickZones {
		if id == "failed-child-000" {
			ys = append(ys, y)
		}
	}
	require.Len(t, ys, 2, "failed row and detail row should both map to the subagent")

	plain := stripANSILines(m.cachedLines)
	detailY := -1
	for i, line := range plain {
		if strings.Contains(line, "detail line") {
			detailY = i
			break
		}
	}
	require.NotEqual(t, -1, detailY)
	result, got := m.HandleClickType(m.layoutCfg.PaddingLeft+2, detailY)
	require.Equal(t, ClickSubagent, result)
	require.Equal(t, "failed-child-000", got)
	require.True(t, m.updateSubagentHoverAt(m.layoutCfg.PaddingLeft+2, detailY))
	require.Equal(t, "failed-child-000", m.hoveredSubagentID)
}

func subagentZoneY(t *testing.T, m *model, id string) int {
	t.Helper()
	for y, got := range m.subagentClickZones {
		if got == id {
			return y - m.scrollview.ScrollOffset()
		}
	}
	t.Fatalf("subagent zone for %s not found", id)
	return 0
}

func findLineContaining(t *testing.T, lines []string, want string) string {
	t.Helper()
	for _, line := range lines {
		if strings.Contains(line, want) {
			return line
		}
	}
	t.Fatalf("line containing %q not found in %#v", want, lines)
	return ""
}

func stripANSILines(lines []string) []string {
	out := make([]string, len(lines))
	for i, line := range lines {
		out[i] = ansi.Strip(line)
	}
	return out
}
