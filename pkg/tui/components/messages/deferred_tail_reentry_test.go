package messages

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func deferredTailFixture(t *testing.T) (*model, string) {
	t.Helper()
	m := NewScrollableView(animation.NewRuntime(), 60, 8, &service.SessionState{}).(*model)
	m.SetSize(60, 8)
	msg := types.Agent(types.MessageTypeAssistant, "root", strings.Repeat("history line\n", 40)+"tail-start\n")
	m.messages = append(m.messages, msg)
	m.views = append(m.views, m.createMessageView(msg))
	_ = m.View()
	m.scrollToTop()
	chunk := strings.Repeat("deferred marker line\n", 16)
	m.AppendToLastMessage("root", chunk)
	require.NotEmpty(t, m.deferredTail)
	return m, chunk
}

func TestDeferredTailMaterializesWhenDownwardRangeIntersects(t *testing.T) {
	for _, tc := range []struct {
		name string
		move func(*model)
	}{
		{"line-down", func(m *model) { m.scrollDown() }},
		{"page-down", func(m *model) { m.scrollPageDown() }},
		{"wheel-down", func(m *model) { m.scrollByWheel(1) }},
		{"end", func(m *model) { m.scrollToBottom() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, chunk := deferredTailFixture(t)
			if tc.name != "end" {
				// Put the requested viewport/overscan immediately before the
				// stale final item so this operation is the reentry boundary.
				start := m.lineOffsets[m.deferredTailIndex]
				m.setScrollOffset(max(0, start-m.height))
			}
			tc.move(m)
			require.Empty(t, m.deferredTail)
			require.True(t, strings.HasSuffix(m.messages[len(m.messages)-1].Content, chunk))
			var exactLines []string
			for i := range m.totalHeight {
				exactLines = append(exactLines, m.renderedLine(i))
			}
			require.Contains(t, strings.Join(exactLines, "\n"), "deferred marker line")
		})
	}
}

func TestDeferredTailMaterializesBeforeScrollbarGeometryAndDrag(t *testing.T) {
	m, chunk := deferredTailFixture(t)
	x := m.scrollview.ScrollbarX()
	_, _ = m.handleMouseClick(tea.MouseClickMsg{X: x, Y: m.yPos, Button: tea.MouseLeft})
	require.Empty(t, m.deferredTail, "scrollbar click must use exact total height")
	require.True(t, strings.HasSuffix(m.messages[0].Content, chunk))

	// A motion after grabbing the thumb continues to use reconciled geometry.
	_, _ = m.handleMouseMotion(tea.MouseMotionMsg{X: x, Y: m.yPos + m.height - 1})
	_, _ = m.handleMouseRelease(tea.MouseReleaseMsg{X: x, Y: m.yPos + m.height - 1, Button: tea.MouseLeft})
	require.False(t, m.scrollview.IsDragging())
}

func TestFinalizeStreamMaterializesDeferredTailWithoutJumping(t *testing.T) {
	m, chunk := deferredTailFixture(t)
	offset := m.scrollOffset
	m.FinalizeStream()
	require.Empty(t, m.deferredTail)
	require.Equal(t, offset, m.scrollOffset, "finalization preserves the scrolled-up viewport")
	require.True(t, strings.HasSuffix(m.messages[0].Content, chunk))
}
