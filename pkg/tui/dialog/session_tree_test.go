package dialog

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
)

// strip is a test helper that removes ANSI sequences so assertions can be
// written against the plain text layout of a rendered line.
func strip(s string) string { return ansi.Strip(s) }

// TestBuildSessionTreeLines_EmptyNodes exercises the guard path: callers who
// pass no nodes get an empty slice back rather than a panic or a "root" line
// made out of thin air.
func TestBuildSessionTreeLines_EmptyNodes(t *testing.T) {
	t.Parallel()

	got := buildSessionTreeLines(nil, "root-1", time.Now())
	assert.Empty(t, got, "empty node list must not fabricate rows")
}

// TestBuildSessionTreeLines_RootOnly covers the most common case: one owned
// session with no live subagents. The renderer should still produce a single
// useful row so the dialog never feels broken.
func TestBuildSessionTreeLines_RootOnly(t *testing.T) {
	t.Parallel()

	now := time.Now()
	root := runtime.LiveSessionNode{
		SessionID: "root-1", RootSessionID: "root-1",
		Kind: runtime.LiveSessionRoot, AgentName: "orchestrator",
		Title: "Initial conversation", Status: "running",
		CreatedAt: now.Add(-30 * time.Second),
	}

	lines := buildSessionTreeLines([]runtime.LiveSessionNode{root}, "root-1", now)
	require.NotEmpty(t, lines, "root-only tree should still render something")
	plain := strings.Join(lines, "\n")
	plain = strip(plain)
	assert.Contains(t, plain, "orchestrator", "root agent name must be shown")
	assert.Contains(t, plain, "working", "root nodes always read as working in the dialog")
	assert.Contains(t, plain, "← you are here",
		"the active tab's node is marked so the user can orient themselves")
}

// TestBuildSessionTreeLines_NestedStructure is the core test for the feature
// we're adding: sub-sessions can spawn more sub-sessions and the tree must
// visualize those relationships correctly, with stable tree-drawing glyphs
// that distinguish "last sibling" from "not last sibling".
func TestBuildSessionTreeLines_NestedStructure(t *testing.T) {
	t.Parallel()

	now := time.Now()
	nodes := []runtime.LiveSessionNode{
		{SessionID: "root-1", RootSessionID: "root-1", Kind: runtime.LiveSessionRoot, AgentName: "orchestrator", Status: "running", CreatedAt: now.Add(-5 * time.Minute)},
		{SessionID: "planner-1", ParentSessionID: "root-1", RootSessionID: "root-1", Kind: runtime.LiveSessionSubAgent, AgentName: "planner", Status: "running", CreatedAt: now.Add(-4 * time.Minute)},
		{SessionID: "researcher-1", ParentSessionID: "planner-1", RootSessionID: "root-1", Kind: runtime.LiveSessionSubAgent, AgentName: "researcher", Status: "waiting", CreatedAt: now.Add(-3 * time.Minute)},
		{SessionID: "writer-1", ParentSessionID: "planner-1", RootSessionID: "root-1", Kind: runtime.LiveSessionSubAgent, AgentName: "writer", Status: "failed", CreatedAt: now.Add(-2 * time.Minute), Error: "unexpected EOF"},
		{SessionID: "reviewer-1", ParentSessionID: "root-1", RootSessionID: "root-1", Kind: runtime.LiveSessionSubAgent, AgentName: "reviewer", Status: "closed", CreatedAt: now.Add(-time.Minute)},
	}

	lines := buildSessionTreeLines(nodes, "researcher-1", now)
	require.NotEmpty(t, lines)

	// Collapse to plain text so we can reason about layout / ordering.
	plain := make([]string, len(lines))
	for i, l := range lines {
		plain[i] = strip(l)
	}
	joined := strings.Join(plain, "\n")

	// Tree shape assertions. Every concept the user cares about must be
	// visible at least once; we intentionally don't pin exact line numbers
	// because the renderer may split a node into a primary + detail line.
	assert.Contains(t, joined, "orchestrator", "root node renders")
	assert.Contains(t, joined, "planner", "planner subagent renders")
	assert.Contains(t, joined, "researcher", "nested researcher renders")
	assert.Contains(t, joined, "writer", "nested writer renders")
	assert.Contains(t, joined, "reviewer", "reviewer sibling renders")

	// The nested node (researcher) lives under planner. We want the
	// researcher row's prefix to include both an ancestor stem and a
	// branch connector — that's the whole point of the tree renderer.
	var researcherLine string
	for _, l := range plain {
		if strings.Contains(l, "researcher") && !strings.Contains(l, "writer") {
			researcherLine = l
			break
		}
	}
	require.NotEmpty(t, researcherLine)
	assert.True(t,
		strings.Contains(researcherLine, "├") || strings.Contains(researcherLine, "└"),
		"nested rows should carry a tree connector; got %q", researcherLine)
	assert.Contains(t, researcherLine, "│",
		"researcher sits under planner which still has more siblings, so the column stem must appear")
	assert.Contains(t, researcherLine, "← you are here",
		"the current session id must be marked even when it lives deep in the tree")

	// The writer failed; its detail line must surface the error so users
	// don't need to hunt for it.
	assert.Contains(t, joined, "unexpected EOF",
		"failed nodes must expose their error detail inline")
	assert.Contains(t, joined, "failed",
		"failed status chip must be present")

	// Reviewer is the last child of the root (by CreatedAt), so its prefix
	// should end with `└─` and never introduce a residual stem below it.
	var reviewerLine string
	for _, l := range plain {
		if strings.Contains(l, "reviewer") {
			reviewerLine = l
			break
		}
	}
	require.NotEmpty(t, reviewerLine)
	assert.Contains(t, reviewerLine, "└─",
		"last sibling of the root should close the branch with └─; got %q", reviewerLine)
}

// TestBuildSessionTreeLines_SiblingOrderDeterministic pins the sort order so
// future changes to the renderer can't silently scramble the tree under the
// user's feet.
func TestBuildSessionTreeLines_SiblingOrderDeterministic(t *testing.T) {
	t.Parallel()

	now := time.Now()
	nodes := []runtime.LiveSessionNode{
		{SessionID: "root-1", Kind: runtime.LiveSessionRoot, AgentName: "root", Status: "running", CreatedAt: now.Add(-10 * time.Minute)},
		{SessionID: "c", ParentSessionID: "root-1", Kind: runtime.LiveSessionSubAgent, AgentName: "gamma", Status: "running", CreatedAt: now.Add(-3 * time.Minute)},
		{SessionID: "a", ParentSessionID: "root-1", Kind: runtime.LiveSessionSubAgent, AgentName: "alpha", Status: "running", CreatedAt: now.Add(-5 * time.Minute)},
		{SessionID: "b", ParentSessionID: "root-1", Kind: runtime.LiveSessionSubAgent, AgentName: "beta", Status: "running", CreatedAt: now.Add(-4 * time.Minute)},
	}

	lines := buildSessionTreeLines(nodes, "root-1", now)
	require.GreaterOrEqual(t, len(lines), 4)

	// First line is the root; direct children follow in CreatedAt order.
	plain := make([]string, len(lines))
	for i, l := range lines {
		plain[i] = strip(l)
	}
	var order []string
	for _, name := range []string{"alpha", "beta", "gamma"} {
		for _, l := range plain {
			if strings.Contains(l, name) {
				order = append(order, name)
				break
			}
		}
	}
	assert.Equal(t, []string{"alpha", "beta", "gamma"}, order,
		"siblings should be rendered in CreatedAt order")
}

// TestSessionTreeStatusLabel_NormalizesVocab locks down the dialog's
// user-facing status vocabulary. We intentionally flatten subagent.Status
// into a shorter word set here because users care about the lifecycle
// (working / idle / finalized / ended / failed) more than the raw runtime
// state names.
func TestSessionTreeStatusLabel_NormalizesVocab(t *testing.T) {
	t.Parallel()

	cases := []struct {
		status string
		want   string
	}{
		{"running", "working"},
		{"starting", "working"},
		{"waiting", "idle"},
		{"closed", "finalized"},
		{"stopped", "ended"},
		{"failed", "failed"},
		{"", "unknown"},
		{"someday", "someday"},
	}
	for _, tc := range cases {
		got := sessionTreeStatusLabel(runtime.LiveSessionNode{Kind: runtime.LiveSessionSubAgent, Status: tc.status})
		assert.Equal(t, tc.want, got, "status %q", tc.status)
	}

	// Root sessions always read as working, regardless of status string —
	// this dialog is only ever opened while a session is alive.
	got := sessionTreeStatusLabel(runtime.LiveSessionNode{Kind: runtime.LiveSessionRoot, Status: "whatever"})
	assert.Equal(t, "working", got)
}

// TestTreePrefix_ShapeAtDepth pins the prefix shape for both the primary and
// the detail-line variants. The detail-line prefix must sit under the
// connector so two-line nodes look like one contiguous block.
func TestTreePrefix_ShapeAtDepth(t *testing.T) {
	t.Parallel()

	// First-level child, not last.
	assert.Equal(t, "├─ ", treePrefix(nil, false, false))
	assert.Equal(t, "│  ", treePrefix(nil, false, true))

	// First-level child, last.
	assert.Equal(t, "└─ ", treePrefix(nil, true, false))
	assert.Equal(t, "   ", treePrefix(nil, true, true))

	// Grandchild under an ancestor that still has more siblings below it.
	assert.Equal(t, "│  ├─ ", treePrefix([]bool{true}, false, false))
	assert.Equal(t, "│  │  ", treePrefix([]bool{true}, false, true))

	// Grandchild under the last ancestor (no stem in the column above).
	assert.Equal(t, "   └─ ", treePrefix([]bool{false}, true, false))
}

// TestBuildSessionTree_RowsMapLinesToNodes verifies the row table produced by
// [buildSessionTree]. Callers rely on this mapping for click + selection
// handling, so it has to stay aligned with the rendered lines even when a
// node contributes a secondary detail line (e.g. a failed node's error).
func TestBuildSessionTree_RowsMapLinesToNodes(t *testing.T) {
	t.Parallel()

	now := time.Now()
	nodes := []runtime.LiveSessionNode{
		{SessionID: "root", RootSessionID: "root", Kind: runtime.LiveSessionRoot, AgentName: "root", Status: "running", CreatedAt: now.Add(-10 * time.Minute)},
		{SessionID: "planner", ParentSessionID: "root", Kind: runtime.LiveSessionSubAgent, AgentName: "planner", Status: "running", CreatedAt: now.Add(-5 * time.Minute)},
		{SessionID: "writer", ParentSessionID: "planner", Kind: runtime.LiveSessionSubAgent, AgentName: "writer", Status: "failed", Error: "boom", CreatedAt: now.Add(-time.Minute)},
	}

	lines, rows := buildSessionTree(nodes, "root", "planner", now, 100)
	require.NotEmpty(t, lines)
	require.Len(t, rows, 3, "one row per node, regardless of secondary detail lines")

	// Every row's line range must stay within the rendered line slice and
	// not overlap with the next row. A writer's failed detail line should
	// make its range span two lines.
	for i, r := range rows {
		require.GreaterOrEqual(t, r.FirstLine, 0)
		require.GreaterOrEqual(t, r.LastLine, r.FirstLine)
		require.Less(t, r.LastLine, len(lines))
		if i+1 < len(rows) {
			assert.Less(t, r.LastLine, rows[i+1].FirstLine,
				"rows must not overlap in the rendered line slice")
		}
	}

	writer := rows[2]
	assert.Equal(t, "writer", writer.Node.SessionID)
	assert.Greater(t, writer.LastLine, writer.FirstLine,
		"failed writer should render a primary + detail line and span two lines")
}

// TestSessionTreeDialog_InitialSelectionMatchesCurrentSession locks in the UX
// choice that opening the dialog starts with the current session highlighted.
// Users should be able to press Enter immediately to focus their current tab
// or arrow away without losing context.
func TestSessionTreeDialog_InitialSelectionMatchesCurrentSession(t *testing.T) {
	t.Parallel()

	now := time.Now()
	nodes := []runtime.LiveSessionNode{
		{SessionID: "root", Kind: runtime.LiveSessionRoot, AgentName: "root", Status: "running", CreatedAt: now.Add(-5 * time.Minute)},
		{SessionID: "a", ParentSessionID: "root", Kind: runtime.LiveSessionSubAgent, AgentName: "a", Status: "running", CreatedAt: now.Add(-4 * time.Minute)},
		{SessionID: "b", ParentSessionID: "root", Kind: runtime.LiveSessionSubAgent, AgentName: "b", Status: "running", CreatedAt: now.Add(-3 * time.Minute)},
	}

	dlg := NewSessionTreeDialog(nodes, "root", "b").(*sessionTreeDialog)
	_ = dlg.SetSize(120, 40)
	_ = dlg.View() // force rows to build

	require.Len(t, dlg.rows, 3)
	assert.Equal(t, "b", dlg.selectedNodeID,
		"initial selection should track the session the dialog was opened from")
	assert.Equal(t, "b", dlg.rows[dlg.selected].Node.SessionID)
}

// TestSessionTreeDialog_ArrowKeysMoveSelection pins the keyboard flow. The
// dialog must be navigable without a mouse; otherwise users on terminals that
// don't report clicks couldn't use it at all.
func TestSessionTreeDialog_ArrowKeysMoveSelection(t *testing.T) {
	t.Parallel()

	now := time.Now()
	nodes := []runtime.LiveSessionNode{
		{SessionID: "root", Kind: runtime.LiveSessionRoot, AgentName: "root", Status: "running", CreatedAt: now.Add(-5 * time.Minute)},
		{SessionID: "a", ParentSessionID: "root", Kind: runtime.LiveSessionSubAgent, AgentName: "a", Status: "running", CreatedAt: now.Add(-4 * time.Minute)},
		{SessionID: "b", ParentSessionID: "root", Kind: runtime.LiveSessionSubAgent, AgentName: "b", Status: "running", CreatedAt: now.Add(-3 * time.Minute)},
	}

	dlg := NewSessionTreeDialog(nodes, "root", "root").(*sessionTreeDialog)
	_ = dlg.SetSize(120, 40)
	_ = dlg.View()
	require.Equal(t, "root", dlg.rows[dlg.selected].Node.SessionID)

	_, _ = dlg.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	_ = dlg.View()
	assert.Equal(t, "a", dlg.selectedNodeID, "down should move to the next sibling")

	_, _ = dlg.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	_ = dlg.View()
	assert.Equal(t, "b", dlg.selectedNodeID)

	// Clamping past the end must not move off the list.
	_, _ = dlg.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	_ = dlg.View()
	assert.Equal(t, "b", dlg.selectedNodeID, "down past the last row should clamp, not wrap")

	_, _ = dlg.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	_ = dlg.View()
	assert.Equal(t, "a", dlg.selectedNodeID)
}

// TestSessionTreeDialog_HitTestRowMatchesRenderedSelection verifies the
// click-mapping logic directly: when the dialog renders a visible row, a click
// on that row's screen Y coordinate resolves back to the correct node index.
// This is the critical piece for making the dialog clickable.
func TestSessionTreeDialog_HitTestRowMatchesRenderedSelection(t *testing.T) {
	t.Parallel()

	now := time.Now()
	nodes := []runtime.LiveSessionNode{
		{SessionID: "root", Kind: runtime.LiveSessionRoot, AgentName: "root", Status: "running", CreatedAt: now.Add(-5 * time.Minute)},
		{SessionID: "planner", ParentSessionID: "root", Kind: runtime.LiveSessionSubAgent, AgentName: "planner", Status: "running", CreatedAt: now.Add(-4 * time.Minute)},
	}

	dlg := NewSessionTreeDialog(nodes, "root", "root").(*sessionTreeDialog)
	_ = dlg.SetSize(120, 40)
	_ = dlg.View()
	require.Len(t, dlg.rows, 2)

	dialogRow, _ := dlg.Position()
	listStart := dialogRow + 2 + sessionTreeHeaderLines
	plannerY := listStart + dlg.rows[1].FirstLine
	assert.Equal(t, 1, dlg.hitTestRow(plannerY),
		"clicking the planner row should resolve to the planner node index")
}

// TestSessionTreeDialog_OpenSelectedUsesSelectedNode verifies that once a row
// is selected, the dialog produces a command when asked to open it. We don't
// inspect Bubble Tea's internal sequence wrapper here; that is framework
// detail. The user-level contract we care about is: selection works, hit-test
// works, and openSelected returns a navigation command instead of nil.
func TestSessionTreeDialog_OpenSelectedUsesSelectedNode(t *testing.T) {
	t.Parallel()

	now := time.Now()
	nodes := []runtime.LiveSessionNode{
		{SessionID: "root", Kind: runtime.LiveSessionRoot, AgentName: "root", Status: "running", CreatedAt: now.Add(-5 * time.Minute)},
		{SessionID: "planner", ParentSessionID: "root", Kind: runtime.LiveSessionSubAgent, AgentName: "planner", Status: "running", CreatedAt: now.Add(-4 * time.Minute)},
	}

	dlg := NewSessionTreeDialog(nodes, "root", "root").(*sessionTreeDialog)
	_ = dlg.SetSize(120, 40)
	_ = dlg.View()

	// Select planner and open it.
	dlg.selected = 1
	dlg.selectedNodeID = "planner"
	_, cmd := dlg.openSelected()
	require.NotNil(t, cmd,
		"opening a selected row should emit a command so the top-level TUI can switch/attach that session")
}
