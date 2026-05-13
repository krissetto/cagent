package sidebar

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/service"
)

// TestParentSessionLine_OnlyRendersOnSubSession verifies the parent pointer is
// scoped strictly to attached child-session tabs and stays empty on owned
// (root) tabs, so no layout cost is paid in the common case.
func TestParentSessionLine_OnlyRendersOnSubSession(t *testing.T) {
	t.Parallel()

	t.Run("owned tab returns empty", func(t *testing.T) {
		t.Parallel()
		sb := New(service.NewSessionState(session.New())).(*model)
		assert.Empty(t, sb.parentSessionLine(60))
		assert.Empty(t, sb.parentSessionLineCollapsed())
	})

	t.Run("sub-session tab with a known parent renders the line", func(t *testing.T) {
		t.Parallel()
		sess := session.New(session.WithParentID("root-1"))
		ss := service.NewSessionState(sess)
		ss.SetCurrentAgentName("researcher")
		ss.SetParentAgentName("planner")
		sb := New(ss).(*model)

		line := sb.parentSessionLine(60)
		require.NotEmpty(t, line)
		assert.Contains(t, line, "parent:")
		assert.Contains(t, line, "planner")
		assert.NotContains(t, line, "open",
			"the affordance should stay hidden until the parent row is hovered")

		// On hover, the compact affordance appears as `↩ parent`. The previous
		// label (`↩ open`) over-promised because the sidebar cannot cheaply
		// verify the parent session is still live-attachable; this phrasing
		// just points at the relationship instead.
		sb.hoveredParentLine = true
		hovered := sb.parentSessionLine(60)
		assert.Contains(t, hovered, "↩ parent")
	})

	t.Run("sub-session tab without a parent agent name falls back gracefully", func(t *testing.T) {
		t.Parallel()
		sess := session.New(session.WithParentID("root-1"))
		ss := service.NewSessionState(sess)
		sb := New(ss).(*model)

		line := sb.parentSessionLine(60)
		require.NotEmpty(t, line, "line must still render so the jump target is never hidden")
		assert.Contains(t, line, "parent")
	})
}

// TestParentSessionLineCollapsed_Shape verifies the collapsed variant stays on
// a single line (so the collapsed layout math is predictable).
func TestParentSessionLineCollapsed_Shape(t *testing.T) {
	t.Parallel()

	sess := session.New(session.WithParentID("root-1"))
	ss := service.NewSessionState(sess)
	ss.SetCurrentAgentName("researcher")
	ss.SetParentAgentName("planner")
	sb := New(ss).(*model)

	line := sb.parentSessionLineCollapsed()
	require.NotEmpty(t, line)
	assert.Equal(t, 0, strings.Count(line, "\n"),
		"collapsed parent line must fit on a single line to preserve layout math")
	assert.Contains(t, line, "parent:")
	assert.Contains(t, line, "planner")
}

// TestSessionSection_IncludesParentLineForSubSessions verifies the parent line
// is rendered inside the existing Session tab (no separate "Subagent session"
// block) and only for sub-session tabs.
func TestSessionSection_IncludesParentLineForSubSessions(t *testing.T) {
	t.Parallel()

	t.Run("sub-session", func(t *testing.T) {
		t.Parallel()
		sess := session.New(session.WithParentID("root-1"))
		ss := service.NewSessionState(sess)
		ss.SetCurrentAgentName("researcher")
		ss.SetParentAgentName("planner")
		sb := New(ss).(*model)
		sb.mode = ModeVertical
		sb.width = 50
		sb.sessionTitle = "My subagent session"
		sb.titleGenerated = true

		out := sb.View()
		assert.Contains(t, out, "Session", "the canonical Session tab title must still render")
		assert.NotContains(t, out, "Subagent session",
			"the old separate 'Subagent session' section must be gone; parent info lives inside the Session tab")
		assert.Contains(t, out, "parent:", "parent pointer must render inside the Session tab for sub-sessions")
		assert.Contains(t, out, "planner")
	})

	t.Run("owned tab shows no parent line", func(t *testing.T) {
		t.Parallel()
		sess := session.New()
		ss := service.NewSessionState(sess)
		sb := New(ss).(*model)
		sb.mode = ModeVertical
		sb.width = 50
		sb.sessionTitle = "Root session"
		sb.titleGenerated = true

		out := sb.View()
		assert.NotContains(t, out, "parent:", "owned (root) tabs must not show a parent pointer")
	})
}

// TestHandleClickType_ParentLine_Vertical confirms a click on the parent line
// resolves to ClickParent with the parent session id.
func TestHandleClickType_ParentLine_Vertical(t *testing.T) {
	t.Parallel()

	sess := session.New(session.WithParentID("root-1"))
	ss := service.NewSessionState(sess)
	ss.SetCurrentAgentName("researcher")
	ss.SetParentAgentName("planner")
	sb := New(ss).(*model)

	sb.mode = ModeVertical
	sb.width = 50
	sb.sessionHasContent = true
	sb.titleGenerated = true
	sb.sessionTitle = "Hi"
	sb.workingDirectory = "~/projects/myapp"

	// Render first so post-render click zones are built.
	_ = sb.View()

	paddingLeft := sb.layoutCfg.PaddingLeft
	titleLines := sb.titleLineCount()
	wdY := verticalStarY + titleLines + 1 // title block + blank separator
	parentY := wdY + 2                    // wd + blank spacer above parent line

	result, _ := sb.HandleClickType(paddingLeft+3, wdY)
	assert.Equal(t, ClickWorkingDir, result,
		"working dir sits directly under the title block, above the parent line")

	result, id := sb.HandleClickType(paddingLeft+3, parentY)
	assert.Equal(t, ClickParent, result, "parent line must be clickable")
	assert.Equal(t, "root-1", id, "click payload must carry the parent session id")
}

// TestHandleClickType_ParentLine_Vertical_WdWraps guards against the off-by-N
// bug where a working-directory line that wraps to multiple lines pushed the
// parent row down further than the fixed +2 offset assumed.
func TestHandleClickType_ParentLine_Vertical_WdWraps(t *testing.T) {
	t.Parallel()

	sess := session.New(session.WithParentID("root-1"))
	ss := service.NewSessionState(sess)
	ss.SetCurrentAgentName("researcher")
	ss.SetParentAgentName("planner")
	sb := New(ss).(*model)

	sb.mode = ModeVertical
	sb.width = 25 // narrow width forces long working dir to wrap
	sb.sessionHasContent = true
	sb.titleGenerated = true
	sb.sessionTitle = "Hi"
	sb.workingDirectory = "~/very/long/path/that/wraps/across/many/lines"

	// Render first so post-render click zones are built.
	_ = sb.View()

	paddingLeft := sb.layoutCfg.PaddingLeft

	// Regardless of how many lines the working directory occupies, at least
	// one click zone must resolve to ClickParent.
	found := false
	for y := range 30 {
		result, id := sb.HandleClickType(paddingLeft+1, y)
		if result == ClickParent {
			assert.Equal(t, "root-1", id)
			found = true
			break
		}
	}
	assert.True(t, found, "parent line must be clickable even when working directory wraps")
}

// TestHandleClickType_ParentLine_Collapsed verifies the same behaviour in
// collapsed mode.
func TestHandleClickType_ParentLine_Collapsed(t *testing.T) {
	t.Parallel()

	sess := session.New(session.WithParentID("root-1"))
	ss := service.NewSessionState(sess)
	ss.SetCurrentAgentName("researcher")
	ss.SetParentAgentName("planner")
	sb := New(ss).(*model)

	sb.mode = ModeCollapsed
	sb.width = 50
	sb.sessionHasContent = true
	sb.titleGenerated = true
	sb.sessionTitle = "Hi"
	sb.workingDirectory = "~/projects/myapp"

	paddingLeft := sb.layoutCfg.PaddingLeft

	// Title is at y=0, working dir at y=1, blank spacer at y=2, parent line at y=3.
	result, _ := sb.HandleClickType(paddingLeft+3, 1)
	assert.Equal(t, ClickWorkingDir, result,
		"working dir should stay directly below the title section in collapsed mode")

	result, id := sb.HandleClickType(paddingLeft+3, 3)
	assert.Equal(t, ClickParent, result)
	assert.Equal(t, "root-1", id)
}
