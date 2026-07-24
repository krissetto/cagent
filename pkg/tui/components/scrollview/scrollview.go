// Package scrollview provides a composable scrollable view that pairs content
// with a fixed-position scrollbar.
//
// Simple path: call [Model.Update] + [Model.View].
// Advanced path (custom scroll management): use [Model.UpdateMouse],
// [Model.SetScrollOffset], and [Model.ViewWithLines].
package scrollview

import (
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/components/scrollbar"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// ScrollKeyMap defines which keys trigger scroll actions.
type ScrollKeyMap struct {
	Up       key.Binding // optional — leave unset for list dialogs that use up/down for selection
	Down     key.Binding
	PageUp   key.Binding
	PageDown key.Binding
	Top      key.Binding // home
	Bottom   key.Binding // end
}

// DefaultScrollKeyMap returns a key map with page-up/down and home/end.
// Up/Down are intentionally unbound so list dialogs can use them for selection.
func DefaultScrollKeyMap() *ScrollKeyMap {
	return &ScrollKeyMap{
		PageUp:   key.NewBinding(key.WithKeys("pgup")),
		PageDown: key.NewBinding(key.WithKeys("pgdown")),
		Top:      key.NewBinding(key.WithKeys("home")),
		Bottom:   key.NewBinding(key.WithKeys("end")),
	}
}

// ReadOnlyScrollKeyMap returns a key map where up/down/j/k also scroll.
func ReadOnlyScrollKeyMap() *ScrollKeyMap {
	return &ScrollKeyMap{
		Up:       key.NewBinding(key.WithKeys("up", "k")),
		Down:     key.NewBinding(key.WithKeys("down", "j")),
		PageUp:   key.NewBinding(key.WithKeys("pgup")),
		PageDown: key.NewBinding(key.WithKeys("pgdown")),
		Top:      key.NewBinding(key.WithKeys("home")),
		Bottom:   key.NewBinding(key.WithKeys("end")),
	}
}

type Option func(*Model)

// WithGapWidth sets the space columns between content and scrollbar (default 1).
func WithGapWidth(n int) Option { return func(m *Model) { m.gapWidth = max(0, n) } }

// WithReserveScrollbarSpace always reserves gap+scrollbar columns, preventing layout shifts.
func WithReserveScrollbarSpace(v bool) Option {
	return func(m *Model) { m.reserveScrollbarSpace = v }
}

// WithWheelStep sets lines scrolled per wheel tick (default 2).
func WithWheelStep(n int) Option { return func(m *Model) { m.wheelStep = n } }

// WithKeyMap sets keyboard bindings for scroll actions. Pass nil to disable.
func WithKeyMap(km *ScrollKeyMap) Option { return func(m *Model) { m.keyMap = km } }

// WithFadeEffect enables a fade-out gradient on edge lines when content
// extends beyond the visible area. The number of faded lines scales
// automatically with the viewport height.
func WithFadeEffect() Option {
	return func(m *Model) { m.fadeEffect = true }
}

// WithFadeEffectDisabled disables the edge fade effect for this scroll view.
func WithFadeEffectDisabled() Option {
	return func(m *Model) { m.fadeEffect = false }
}

// ScrollbarDecorator transforms only the rendered scrollbar view before it is
// composed into the scrollview. It is opt-in so existing scrollviews retain
// their current output and cache behavior.
type ScrollbarDecorator func(string) string

// WithScrollbarDecorator sets an optional rendered-scrollbar decorator.
func WithScrollbarDecorator(decorator ScrollbarDecorator) Option {
	return func(m *Model) { m.scrollbarDecorator = decorator }
}

// Model is a composable scrollable view that owns a scrollbar and ensures
// fixed-width rendering.
type Model struct {
	sb *scrollbar.Model

	xPos, yPos    int
	width, height int

	gapWidth              int
	reserveScrollbarSpace bool
	wheelStep             int
	keyMap                *ScrollKeyMap

	lines       []string
	totalHeight int

	// lineWidths lazily caches the display width of each content line. It is
	// retained when SetContent receives the same backing slice and invalidated
	// when content identity changes or explicit invalidation is requested.
	lineWidths []int

	// View output cache for pre-sliced/restyled callers. The copied input is a
	// stable cache key: callers may reuse and mutate their viewport slice, so
	// pointer identity alone is insufficient.
	lastViewLines       []string
	lastViewOutput      string
	lastViewOffset      int
	lastViewWidth       int
	lastViewHeight      int
	lastViewTotalHeight int
	lastViewFade        bool
	viewCacheDirty      bool

	fadeEffect bool

	scrollbarDecorator ScrollbarDecorator

	// scrollOffset tracks the desired scroll position independently of the
	// scrollbar, so EnsureLineVisible works before SetContent is called.
	scrollOffset int
}

// New creates a new scrollview with the given options.
func New(opts ...Option) *Model {
	m := &Model{
		sb:         scrollbar.New(),
		gapWidth:   1,
		wheelStep:  2,
		keyMap:     DefaultScrollKeyMap(),
		fadeEffect: true,
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// SetSize sets the total width and height of the scrollable region.
func (m *Model) SetSize(width, height int) {
	if m.width != width || m.height != height {
		m.viewCacheDirty = true
	}
	m.width = width
	m.height = height
	m.updateScrollbarPosition()
}

// SetPosition sets the absolute screen position (for mouse hit-testing).
func (m *Model) SetPosition(x, y int) {
	m.xPos = x
	m.yPos = y
	m.updateScrollbarPosition()
}

// SetContent provides the full content buffer and total height.
// totalHeight may be >= len(lines) for virtual blank lines (e.g. bottomSlack).
func (m *Model) SetContent(lines []string, totalHeight int) {
	nextTotalHeight := max(totalHeight, len(lines))
	if len(lines) != len(m.lines) || (len(lines) > 0 && &lines[0] != &m.lines[0]) {
		m.lineWidths = nil
		m.viewCacheDirty = true
	}
	if nextTotalHeight != m.totalHeight {
		m.viewCacheDirty = true
	}
	m.lines = lines
	m.totalHeight = nextTotalHeight
	m.sb.SetDimensions(m.height, m.totalHeight)
}

// lineWidth returns the display width of content line i, memoized across frames.
func (m *Model) lineWidth(i int) int {
	if m.lineWidths == nil {
		m.lineWidths = make([]int, len(m.lines))
		for j := range m.lineWidths {
			m.lineWidths[j] = -1
		}
	}
	if w := m.lineWidths[i]; w >= 0 {
		return w
	}
	w := ansi.StringWidth(m.lines[i])
	m.lineWidths[i] = w
	return w
}

// NeedsScrollbar returns true if content is taller than the viewport.
func (m *Model) NeedsScrollbar() bool { return m.totalHeight > m.height }

// ContentWidth returns the width available for content text.
func (m *Model) ContentWidth() int {
	if m.reserveScrollbarSpace || m.NeedsScrollbar() {
		return max(1, m.width-m.gapWidth-scrollbar.Width)
	}
	return max(1, m.width)
}

// InvalidateComposeCache invalidates memoized line measurements. Call it when
// content passed to SetContent has changed in place.
func (m *Model) InvalidateComposeCache() {
	m.lineWidths = nil
	m.viewCacheDirty = true
}

// SetScrollbarDecorator sets or clears the optional rendered-scrollbar
// decorator.
func (m *Model) SetScrollbarDecorator(decorator ScrollbarDecorator) {
	m.scrollbarDecorator = decorator
	m.viewCacheDirty = true
}

// ReservedCols returns columns reserved for gap + scrollbar.
func (m *Model) ReservedCols() int { return m.gapWidth + scrollbar.Width }

// VisibleHeight returns the viewport height in lines.
func (m *Model) VisibleHeight() int { return m.height }

// MaxScrollOffset returns the maximum valid scroll offset — 0 when the
// content fits entirely within the viewport.
func (m *Model) MaxScrollOffset() int {
	if m.totalHeight <= m.height {
		return 0
	}
	return m.totalHeight - m.height
}

// ScrollbarX returns the absolute screen X of the scrollbar column.
func (m *Model) ScrollbarX() int { return m.xPos + m.width - scrollbar.Width }

// IsMouseOnScrollbar reports whether screen coordinates hit the scrollbar column.
func (m *Model) IsMouseOnScrollbar(x, y int) bool {
	m.updateScrollbarPosition()
	return x >= m.ScrollbarX() && x < m.ScrollbarX()+scrollbar.Width &&
		y >= m.yPos && y < m.yPos+m.height
}

// ScrollOffset returns the current scroll offset.
func (m *Model) ScrollOffset() int { return m.scrollOffset }

// SetScrollOffset sets the scroll offset, clamped when content dimensions are known.
func (m *Model) SetScrollOffset(offset int) {
	previous := m.scrollOffset
	m.scrollOffset = max(0, offset)
	if m.totalHeight > 0 && m.height > 0 {
		m.scrollOffset = min(m.scrollOffset, max(0, m.totalHeight-m.height))
	}
	if m.scrollOffset != previous {
		m.viewCacheDirty = true
	}
	m.sb.SetScrollOffset(m.scrollOffset)
}

// ScrollBy adjusts the scroll offset by delta lines.
func (m *Model) ScrollBy(delta int) { m.SetScrollOffset(m.scrollOffset + delta) }
func (m *Model) LineUp()            { m.ScrollBy(-1) }
func (m *Model) LineDown()          { m.ScrollBy(1) }
func (m *Model) PageUp()            { m.ScrollBy(-m.height) }
func (m *Model) PageDown()          { m.ScrollBy(m.height) }
func (m *Model) ScrollToTop()       { m.SetScrollOffset(0) }
func (m *Model) ScrollToBottom()    { m.SetScrollOffset(m.totalHeight) }

// IsAtBottom returns true when the viewport is scrolled to the bottom of the
// content, or when the content fits entirely within the viewport.
func (m *Model) IsAtBottom() bool {
	if m.totalHeight <= m.height {
		return true
	}
	return m.scrollOffset >= m.totalHeight-m.height
}

// EnsureLineVisible scrolls minimally to bring a line into the viewport.
// Works before [SetContent] — only needs [SetSize].
func (m *Model) EnsureLineVisible(line int) {
	m.EnsureRangeVisible(line, line)
}

// EnsureRangeVisible scrolls minimally to bring lines startLine..endLine into
// the view. If the range is taller than the view, the start is prioritized.
func (m *Model) EnsureRangeVisible(startLine, endLine int) {
	startLine = max(0, startLine)
	endLine = max(startLine, endLine)
	if endLine >= m.scrollOffset+m.height {
		m.SetScrollOffset(endLine - m.height + 1)
	}
	if startLine < m.scrollOffset {
		m.SetScrollOffset(startLine)
	}
}

// Update handles mouse (scrollbar click/drag/wheel) and keyboard scroll events.
// Returns handled=true when the event was consumed.
func (m *Model) Update(msg tea.Msg) (handled bool, cmd tea.Cmd) {
	m.updateScrollbarPosition() // Ensure scrollbar position is fresh for hit-testing
	switch msg := msg.(type) {
	case tea.MouseClickMsg, tea.MouseMotionMsg, tea.MouseReleaseMsg:
		return m.UpdateMouse(msg)

	case messages.WheelCoalescedMsg:
		if msg.Delta != 0 {
			m.ScrollBy(msg.Delta * m.wheelStep)
			return true, nil
		}

	case tea.MouseWheelMsg:
		switch msg.Button {
		case tea.MouseWheelUp:
			m.ScrollBy(-m.wheelStep)
			return true, nil
		case tea.MouseWheelDown:
			m.ScrollBy(m.wheelStep)
			return true, nil
		}

	case tea.KeyPressMsg:
		if m.keyMap == nil {
			return false, nil
		}
		switch {
		case m.keyMap.Up.Enabled() && key.Matches(msg, m.keyMap.Up):
			m.LineUp()
			return true, nil
		case m.keyMap.Down.Enabled() && key.Matches(msg, m.keyMap.Down):
			m.LineDown()
			return true, nil
		case key.Matches(msg, m.keyMap.PageUp):
			m.PageUp()
			return true, nil
		case key.Matches(msg, m.keyMap.PageDown):
			m.PageDown()
			return true, nil
		case key.Matches(msg, m.keyMap.Top):
			m.ScrollToTop()
			return true, nil
		case key.Matches(msg, m.keyMap.Bottom):
			m.ScrollToBottom()
			return true, nil
		}
	}
	return false, nil
}

// UpdateMouse delegates mouse events to the scrollbar. Low-level alternative to [Update].
func (m *Model) UpdateMouse(msg tea.Msg) (handled bool, cmd tea.Cmd) {
	prev := m.scrollOffset
	sb, c := m.sb.Update(msg)
	m.sb = sb
	m.scrollOffset = m.sb.GetScrollOffset()
	return m.scrollOffset != prev || m.sb.IsDragging(), c
}

// IsDragging returns whether the scrollbar thumb is being dragged.
func (m *Model) IsDragging() bool { return m.sb.IsDragging() }

// View renders the scrollable region with automatic content slicing.
// The output always has exactly m.height lines to ensure consistent layout.
func (m *Model) View() string {
	if m.width <= 0 || m.height <= 0 {
		return ""
	}
	m.syncScrollbar()

	// Always produce exactly m.height lines for consistent layout.
	// This ensures the status bar stays at the bottom of the screen.
	visible := make([]string, m.height)
	for i := range m.height {
		if idx := m.scrollOffset + i; idx < len(m.lines) {
			visible[i] = m.lines[idx]
		}
		// Empty lines are left as empty strings (zero value)
	}

	return m.compose(visible, m.scrollOffset)
}

// ViewWithLines renders pre-sliced visible lines with the scrollbar.
// The output always has exactly m.height lines to ensure consistent layout.
// Note: This method does NOT use the compose cache because the caller provides
// different visible lines each time. If callers need caching, they should
// implement it at their level (like allSessionsTab does).
func (m *Model) ViewWithLines(visibleLines []string) string {
	return m.viewWithLines(visibleLines, -1)
}

// ViewWithRestyledLines is like [Model.ViewWithLines] for callers whose
// visibleLines are sliced from the content set via [Model.SetContent] at the
// current scroll offset (possibly restyled, e.g. selection or hover
// highlights). Unchanged lines reuse memoized width lookups in compose;
// restyled lines are re-measured.
func (m *Model) ViewWithRestyledLines(visibleLines []string) string {
	if m.cachedViewMatches(visibleLines) {
		return m.lastViewOutput
	}
	result := m.viewWithLines(visibleLines, m.scrollOffset)
	m.lastViewLines = append(m.lastViewLines[:0], visibleLines...)
	m.lastViewOutput = result
	m.lastViewOffset = m.scrollOffset
	m.lastViewWidth = m.width
	m.lastViewHeight = m.height
	m.lastViewTotalHeight = m.totalHeight
	m.lastViewFade = m.fadeEffect
	m.viewCacheDirty = false
	return result
}

func (m *Model) cachedViewMatches(lines []string) bool {
	if m.viewCacheDirty || m.lastViewOutput == "" ||
		m.lastViewOffset != m.scrollOffset || m.lastViewWidth != m.width ||
		m.lastViewHeight != m.height || m.lastViewTotalHeight != m.totalHeight ||
		m.lastViewFade != m.fadeEffect || len(m.lastViewLines) != len(lines) {
		return false
	}
	for i := range lines {
		if lines[i] != m.lastViewLines[i] {
			return false
		}
	}
	return true
}

func (m *Model) viewWithLines(visibleLines []string, baseLine int) string {
	if m.width <= 0 || m.height <= 0 {
		return ""
	}
	m.syncScrollbar()

	result := make([]string, m.height)
	copy(result, visibleLines[:min(len(visibleLines), m.height)])
	return m.compose(result, baseLine)
}

// ViewWithPaddedLines renders pre-sliced lines that are already padded/truncated
// to the scrollview content width. This avoids re-measuring ANSI-heavy content
// on callers' hot paths while preserving scrollbar composition.
func (m *Model) ViewWithPaddedLines(visibleLines []string) string {
	if m.width <= 0 || m.height <= 0 {
		return ""
	}
	m.syncScrollbar()

	result := make([]string, m.height)
	copy(result, visibleLines[:min(len(visibleLines), m.height)])
	return m.composePadded(result)
}

// ViewWithPaddedLinesAndOuterPadding renders lines with symmetric app-level
// horizontal padding around the scrollview.
func (m *Model) ViewWithPaddedLinesAndOuterPadding(visibleLines []string, outerPadding int) string {
	return m.ViewWithPaddedLinesAndPadding(visibleLines, outerPadding, outerPadding)
}

// ViewWithPaddedLinesAndPadding renders pre-sliced viewport lines with the
// scrollbar and explicit left/right horizontal padding in one pass. Each
// visible row is padded/truncated before appending the scrollbar column.
func (m *Model) ViewWithPaddedLinesAndPadding(visibleLines []string, leftPadding, rightPadding int) string {
	return m.viewWithPaddedLinesAndPadding(visibleLines, 0, leftPadding, rightPadding)
}

// ViewWithPaddedContentAndPadding renders a full padded content buffer with the
// scrollbar and explicit left/right horizontal padding. The buffer is sliced
// from the current scroll offset before rendering.
func (m *Model) ViewWithPaddedContentAndPadding(contentLines []string, leftPadding, rightPadding int) string {
	return m.viewWithPaddedLinesAndPadding(contentLines, m.scrollOffset, leftPadding, rightPadding)
}

func (m *Model) viewWithPaddedLinesAndPadding(lines []string, start, leftPadding, rightPadding int) string {
	if m.width <= 0 || m.height <= 0 {
		return ""
	}
	m.syncScrollbar()

	totalWidth := m.ContentWidth()
	needsScrollbar := m.NeedsScrollbar()
	needsReserved := !needsScrollbar && m.reserveScrollbarSpace
	rightWidth := 0
	if needsScrollbar {
		rightWidth = m.gapWidth + scrollbar.Width
	} else if needsReserved {
		rightWidth = m.gapWidth + scrollbar.Width
	}

	leftPad := strings.Repeat(" ", max(0, leftPadding))
	rightPad := strings.Repeat(" ", max(0, rightPadding))
	var rightLines []string
	gap := ""
	if needsScrollbar {
		gap = strings.Repeat(" ", m.gapWidth)
		rightLines = strings.Split(m.decoratedScrollbarView(), "\n")
	} else if needsReserved {
		gap = strings.Repeat(" ", rightWidth)
	}

	fadeLines := fadeLinesForHeight(m.height)
	fadeN := min(fadeLines, m.height/2)
	hasAbove := m.fadeEffect && m.scrollOffset > 0
	hasBelow := m.fadeEffect && m.scrollOffset+m.height < m.totalHeight
	var fc styles.FadeContext
	if fadeN > 0 && (hasAbove || hasBelow) {
		fc = styles.NewFadeContext()
	}

	lineWidth := totalWidth + rightWidth + max(0, leftPadding) + max(0, rightPadding) + 1
	var sb strings.Builder
	sb.Grow(m.height * lineWidth)
	for i := range m.height {
		if i > 0 {
			sb.WriteByte('\n')
		}
		line := ""
		idx := start + i
		if idx >= 0 && idx < len(lines) {
			line = lines[idx]
		}
		line = padOrTruncateANSI(line, totalWidth)
		if fadeN > 0 {
			switch {
			case hasAbove && i < fadeN:
				line = styles.FadeLineCtx(line, fadeAlpha(i, fadeN), &fc)
			case hasBelow && i >= m.height-fadeN:
				line = styles.FadeLineCtx(line, fadeAlpha(m.height-1-i, fadeN), &fc)
			}
			line = padOrTruncateANSI(line, totalWidth)
		}
		sb.WriteString(leftPad)
		sb.WriteString(line)
		if needsScrollbar {
			sb.WriteString(gap)
			if i < len(rightLines) {
				sb.WriteString(rightLines[i])
			} else {
				sb.WriteString(strings.Repeat(" ", scrollbar.Width))
			}
		} else if needsReserved {
			sb.WriteString(gap)
		}
		sb.WriteString(rightPad)
	}

	return sb.String()
}

// syncScrollbar syncs the local scroll offset to the scrollbar and reads back the clamped value.
func (m *Model) syncScrollbar() {
	m.sb.SetDimensions(m.height, m.totalHeight)
	m.sb.SetScrollOffset(m.scrollOffset)
	m.scrollOffset = m.sb.GetScrollOffset()
}

// compose pads/truncates lines to contentWidth and joins with the scrollbar column.
//
// Instead of using lipgloss.JoinHorizontal (which re-parses every ANSI
// sequence to measure column widths), we compose manually in a single pass
// since all column widths are known. This avoids O(totalANSI) re-parsing
// per frame.
func (m *Model) compose(lines []string, baseLine int) string {
	contentWidth := m.ContentWidth()
	result := make([]string, len(lines))

	// Pad or truncate each line to exact content width without mutating the
	// caller's slice. Widths from SetContent are reused when possible.
	for i, line := range lines {
		var w int
		if gi := baseLine + i; baseLine >= 0 && gi < len(m.lines) && line == m.lines[gi] {
			w = m.lineWidth(gi)
		} else {
			w = ansi.StringWidth(line)
		}
		switch {
		case w > contentWidth:
			result[i] = ansi.Truncate(line, contentWidth, "")
		case w < contentWidth:
			result[i] = line + strings.Repeat(" ", contentWidth-w)
		default:
			result[i] = line
		}
	}

	return m.composePadded(result)
}

func padOrTruncateANSI(line string, width int) string {
	if width <= 0 {
		return ""
	}
	w := ansi.StringWidth(line)
	switch {
	case w > width:
		return ansi.Truncate(line, width, "")
	case w < width:
		return line + strings.Repeat(" ", width-w)
	default:
		return line
	}
}

func (m *Model) composePadded(lines []string) string {
	contentWidth := m.ContentWidth()
	result := append([]string(nil), lines...)

	if m.fadeEffect {
		m.applyFade(result)
	}

	// Determine what goes in the right margin.
	needsScrollbar := m.NeedsScrollbar()
	needsReserved := !needsScrollbar && m.reserveScrollbarSpace

	if !needsScrollbar && !needsReserved {
		// No right column — just join content lines.
		return strings.Join(result, "\n")
	}

	// Pre-split the scrollbar or build a placeholder column.
	// Scrollbar.View() returns m.height lines joined by '\n'.
	var rightLines []string
	var gap string
	if needsScrollbar {
		gap = strings.Repeat(" ", m.gapWidth)
		rightLines = strings.Split(m.decoratedScrollbarView(), "\n")
	} else {
		// Reserve space: gap + scrollbar width filled with spaces.
		gap = strings.Repeat(" ", m.gapWidth+scrollbar.Width)
	}

	// Estimate: each line ≈ contentWidth + gapWidth + scrollbarWidth + 1 (newline)
	lineWidth := contentWidth + m.gapWidth + scrollbar.Width + 1
	var sb strings.Builder
	sb.Grow(len(result) * lineWidth)

	for i, line := range result {
		if i > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(line)
		sb.WriteString(gap)
		if needsScrollbar && i < len(rightLines) {
			sb.WriteString(rightLines[i])
		}
	}

	return sb.String()
}

func (m *Model) updateScrollbarPosition() {
	m.sb.SetPosition(m.ScrollbarX(), m.yPos)
}

func (m *Model) decoratedScrollbarView() string {
	view := m.sb.View()
	if m.scrollbarDecorator == nil {
		return view
	}
	return m.scrollbarDecorator(view)
}
