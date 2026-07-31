package scrollview

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/components/scrollbar"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

// composeReference reproduces the previous compose implementation (measure,
// pad, lipgloss.JoinHorizontal) as an oracle for the optimized version.
func composeReference(m *Model, lines []string) string {
	contentWidth := m.ContentWidth()
	for i, line := range lines {
		w := ansi.StringWidth(line)
		switch {
		case w > contentWidth:
			lines[i] = ansi.Truncate(line, contentWidth, "")
		case w < contentWidth:
			lines[i] = line + strings.Repeat(" ", contentWidth-w)
		}
	}
	contentView := strings.Join(lines, "\n")
	if m.NeedsScrollbar() {
		col := strings.Repeat(" ", m.gapWidth)
		gapLines := make([]string, m.height)
		for i := range gapLines {
			gapLines[i] = col
		}
		return lipgloss.JoinHorizontal(lipgloss.Top, contentView, strings.Join(gapLines, "\n"), m.sb.View())
	}
	if m.reserveScrollbarSpace {
		col := strings.Repeat(" ", m.gapWidth+scrollbar.Width)
		blankLines := make([]string, len(lines))
		for i := range blankLines {
			blankLines[i] = col
		}
		return lipgloss.JoinHorizontal(lipgloss.Top, contentView, strings.Join(blankLines, "\n"))
	}
	return contentView
}

func TestComposeMatchesJoinHorizontal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		reserveSpace bool
		totalHeight  int // > height forces a scrollbar
	}{
		{name: "with scrollbar", totalHeight: 100},
		{name: "with scrollbar and reserved space", reserveSpace: true, totalHeight: 100},
		{name: "no scrollbar reserved space", reserveSpace: true, totalHeight: 5},
		{name: "no scrollbar no reserve", totalHeight: 5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := New(WithReserveScrollbarSpace(tt.reserveSpace))
			m.SetSize(40, 10)

			content := make([]string, tt.totalHeight)
			for i := range content {
				content[i] = strings.Repeat("x", (i%7)+1)
			}
			m.SetContent(content, tt.totalHeight)
			m.SetScrollOffset(3)
			m.syncScrollbar()

			nLines := min(m.height, len(content)-m.scrollOffset)
			lines := append([]string(nil), content[m.scrollOffset:m.scrollOffset+nLines]...)
			if m.NeedsScrollbar() {
				for len(lines) < m.height {
					lines = append(lines, "")
				}
			}

			want := composeReference(m, append([]string(nil), lines...))
			got := m.compose(append([]string(nil), lines...), m.scrollOffset)
			assert.Equal(t, want, got)
		})
	}
}

func TestViewIsStableAcrossFrames(t *testing.T) {
	t.Parallel()

	m := New(WithReserveScrollbarSpace(true))
	m.SetSize(30, 6)

	content := make([]string, 50)
	for i := range content {
		content[i] = strings.Repeat("line", (i%3)+1)
	}
	m.SetContent(content, len(content))
	m.SetScrollOffset(10)

	first := m.View()
	require.NotEmpty(t, first)
	// Repeated frames with unchanged state must render identically (widths
	// are memoized after the first frame).
	assert.Equal(t, first, m.View())

	// Scrolling changes the window but keeps the memoized widths valid.
	m.SetScrollOffset(11)
	shifted := m.View()
	assert.NotEqual(t, first, shifted)

	m.SetScrollOffset(10)
	assert.Equal(t, first, m.View())
}

func TestSetContentInvalidatesWidthCache(t *testing.T) {
	t.Parallel()

	m := New()
	m.SetSize(20, 4)

	m.SetContent([]string{"aa", "bb", "cc", "dd", "ee"}, 5)
	before := m.View()

	// New slice with wider lines: cached widths must not leak through.
	m.SetContent([]string{"aaaaaaaa", "bbbbbbbb", "cccccccc", "dddddddd", "eeeeeeee"}, 5)
	after := m.View()

	assert.NotEqual(t, before, after)
	assert.Contains(t, after, "aaaaaaaa")
}

func TestVisualGenerationTracksEffectivePresentationChanges(t *testing.T) {
	t.Parallel()

	m := New()
	generation := m.VisualGeneration()
	require.False(t, m.Changed(generation))

	m.SetSize(10, 4)
	require.True(t, m.Changed(generation))
	generation = m.VisualGeneration()
	m.SetSize(10, 4)
	require.False(t, m.Changed(generation), "identical size changed generation")

	content := []string{"0", "1", "2", "3", "4", "5", "6", "7"}
	m.SetContent(content, len(content))
	require.True(t, m.Changed(generation))
	generation = m.VisualGeneration()
	m.SetContent(append([]string(nil), content...), len(content))
	require.False(t, m.Changed(generation), "equivalent content changed generation")

	m.SetScrollOffset(1)
	require.True(t, m.Changed(generation))
	generation = m.VisualGeneration()
	m.SetScrollOffset(1)
	require.False(t, m.Changed(generation), "identical offset changed generation")

	m.SetScrollOffset(99)
	generation = m.VisualGeneration()
	m.SetSize(10, 6)
	require.True(t, m.Changed(generation), "size and clamp did not change generation")
	assert.Equal(t, 2, m.ScrollOffset())
}

func TestVisualGenerationIgnoresBoundaryInputAndTracksDrag(t *testing.T) {
	t.Parallel()

	m := New()
	m.SetSize(10, 4)
	content := []string{"0", "1", "2", "3", "4", "5", "6", "7"}
	m.SetContent(content, len(content))

	generation := m.VisualGeneration()
	_, _ = m.Update(messages.WheelCoalescedMsg{Delta: -1})
	require.False(t, m.Changed(generation), "top-boundary wheel changed generation")
	_, _ = m.Update(tea.MouseMotionMsg{X: 9, Y: 2})
	require.False(t, m.Changed(generation), "motion without a drag changed generation")

	_, _ = m.Update(tea.MouseClickMsg{X: 9, Y: 0, Button: tea.MouseLeft})
	require.True(t, m.Changed(generation), "starting a drag did not change generation")
	generation = m.VisualGeneration()
	_, _ = m.Update(tea.MouseMotionMsg{X: 9, Y: 0})
	require.False(t, m.Changed(generation), "no-op drag motion changed generation")
	_, _ = m.Update(tea.MouseMotionMsg{X: 9, Y: 2})
	require.True(t, m.Changed(generation), "effective drag motion did not change generation")

	generation = m.VisualGeneration()
	_, _ = m.Update(tea.MouseReleaseMsg{X: 9, Y: 2, Button: tea.MouseLeft})
	require.True(t, m.Changed(generation), "ending a drag did not change generation")
}

func TestComposeRemeasuresRestyledLines(t *testing.T) {
	t.Parallel()

	m := New()
	m.SetSize(20, 4)
	content := []string{"aa", "bb", "cc", "dd", "ee", "ff"}
	m.SetContent(content, len(content))
	m.SetScrollOffset(0)
	m.View() // warm the width cache

	// A restyled line whose display width differs from the cached original
	// (e.g. width drift on complex grapheme clusters) must be re-measured,
	// not padded using the stale cached width.
	restyled := []string{"aaaa", "bb", "cc", "dd"}
	out := m.ViewWithRestyledLines(restyled)
	for line := range strings.SplitSeq(out, "\n") {
		// Full row = content + gap + scrollbar column.
		assert.Equal(t, m.width, ansi.StringWidth(line))
	}
}
