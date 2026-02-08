// Package dashboard provides a dashboard view that shows an overview of all active agent sessions.
package dashboard

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/cagent/pkg/tui/components/scrollbar"
	"github.com/docker/cagent/pkg/tui/core/layout"
	"github.com/docker/cagent/pkg/tui/messages"
	"github.com/docker/cagent/pkg/tui/service/supervisor"
	"github.com/docker/cagent/pkg/tui/styles"
)

const (
	// Card dimensions
	cardMinWidth = 38
	// cardHeight is the outer height of a card including borders.
	// Border adds 2 (top + bottom), so inner content lines = cardHeight - 2 = 7.
	cardHeight = 9
	cardGapH   = 2
	cardGapV   = 1

	// cardBorderSize is the horizontal space consumed by borders (1 left + 1 right).
	cardBorderSize = 2
	// cardPaddingSize is the horizontal space consumed by padding (1 left + 1 right).
	cardPaddingSize = 2
)

// Dashboard is the interface for the dashboard component.
type Dashboard interface {
	layout.Model
	SetSessions(sessions []supervisor.SessionInfo)
	SelectSession(sessionID string)
	SelectedSessionID() string
}

// model implements the Dashboard interface.
type model struct {
	sessions  []supervisor.SessionInfo
	scrollbar *scrollbar.Model

	// Selection state
	selectedIdx int

	// Double-click tracking
	lastClickIdx  int
	lastClickTime time.Time

	// Layout
	width, height int
	cols          int
	cardWidth     int

	// Scroll state
	scrollOffset int

	// Render cache — avoids re-rendering cards on every frame (scroll, mouse move, etc.)
	cachedLines []string // Full rendered content as flat lines
	cacheDirty  bool     // True when cache needs rebuild
}

// New creates a new dashboard component.
func New() Dashboard {
	return &model{
		scrollbar:    scrollbar.New(),
		selectedIdx:  0,
		lastClickIdx: -1,
		cacheDirty:   true,
	}
}

// invalidateCache marks the render cache as stale, causing a rebuild on next View().
func (m *model) invalidateCache() {
	m.cacheDirty = true
}

// SetSessions updates the session data displayed on the dashboard.
func (m *model) SetSessions(sessions []supervisor.SessionInfo) {
	m.sessions = sessions
	// Clamp selection
	totalItems := len(m.sessions) + 1 // +1 for "New Session" card
	if m.selectedIdx >= totalItems {
		m.selectedIdx = totalItems - 1
	}
	m.recalcLayout()
	m.invalidateCache()
}

// SelectedSessionID returns the session ID of the currently selected card,
// or empty string if the "+ New Session" card is selected.
// SelectSession moves the selection cursor to the card matching the given
// session ID. If no match is found the selection is left unchanged.
func (m *model) SelectSession(sessionID string) {
	for i, s := range m.sessions {
		if s.SessionID == sessionID {
			m.selectedIdx = i
			m.ensureSelectedVisible()
			m.invalidateCache()
			return
		}
	}
}

func (m *model) SelectedSessionID() string {
	if m.selectedIdx >= 0 && m.selectedIdx < len(m.sessions) {
		return m.sessions[m.selectedIdx].SessionID
	}
	return ""
}

// contentWidth returns the usable content width inside a card (after border + padding).
func (m *model) contentWidth() int {
	return m.cardWidth - cardBorderSize - cardPaddingSize
}

// Init initializes the dashboard component.
func (m *model) Init() tea.Cmd {
	return nil
}

// SetSize updates the dashboard dimensions.
func (m *model) SetSize(width, height int) tea.Cmd {
	m.width = width
	m.height = height
	m.recalcLayout()
	m.invalidateCache()
	return nil
}

// gridWidth returns the width available for the card grid (reserves scrollbar space).
// This is the width inside AppStyle padding, minus 2 chars for scrollbar (space + bar).
func (m *model) gridWidth() int {
	return m.width - 2 - 2 // AppStyle padding (2) + scrollbar column (2: space + bar)
}

func (m *model) recalcLayout() {
	if m.width <= 0 {
		return
	}

	gridW := m.gridWidth()

	// Calculate columns and card width
	m.cols = max(1, (gridW+cardGapH)/(cardMinWidth+cardGapH))
	if m.cols > 3 {
		m.cols = 3
	}
	m.cardWidth = (gridW - (m.cols-1)*cardGapH) / m.cols

	// Update scrollbar dimensions and fixed position on the right edge.
	// Position: inside AppStyle left padding (1) + grid width + spacer (1).
	totalHeight := m.totalContentHeight()
	m.scrollbar.SetDimensions(m.height, totalHeight)
	m.scrollbar.SetScrollOffset(m.scrollOffset)
	m.scrollbar.SetPosition(1+gridW+1, 0)

	// Clamp scroll
	maxScroll := max(0, totalHeight-m.height)
	if m.scrollOffset > maxScroll {
		m.scrollOffset = maxScroll
	}
}

func (m *model) totalContentHeight() int {
	totalItems := len(m.sessions) + 1 // +1 for new session card
	rows := (totalItems + m.cols - 1) / m.cols
	// Title (1) + blank (1) + rows * (cardHeight + cardGapV) - last gap + bottom padding (1)
	return 2 + rows*(cardHeight+cardGapV)
}

// Update handles messages.
func (m *model) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case messages.ThemeChangedMsg:
		m.invalidateCache()
		return m, nil
	case tea.KeyPressMsg:
		return m.handleKeyPress(msg)
	case tea.MouseClickMsg:
		return m.handleMouseClick(msg)
	case tea.MouseWheelMsg:
		return m.handleMouseWheel(msg)
	case messages.WheelCoalescedMsg:
		return m.handleWheelCoalesced(msg)
	case tea.MouseMotionMsg:
		sbModel, cmd := m.scrollbar.Update(msg)
		m.scrollbar = sbModel
		if m.scrollbar.IsDragging() {
			m.scrollOffset = m.scrollbar.GetScrollOffset()
		}
		return m, cmd
	case tea.MouseReleaseMsg:
		sbModel, cmd := m.scrollbar.Update(msg)
		m.scrollbar = sbModel
		return m, cmd
	}
	return m, nil
}

func (m *model) handleKeyPress(msg tea.KeyPressMsg) (layout.Model, tea.Cmd) {
	totalItems := len(m.sessions) + 1
	prevSelected := m.selectedIdx

	switch msg.Key().Code {
	case tea.KeyUp, 'k':
		if m.selectedIdx >= m.cols {
			m.selectedIdx -= m.cols
		}
	case tea.KeyDown, 'j':
		if m.selectedIdx+m.cols < totalItems {
			m.selectedIdx += m.cols
		}
	case tea.KeyLeft, 'h':
		if m.selectedIdx > 0 {
			m.selectedIdx--
		}
	case tea.KeyRight, 'l':
		if m.selectedIdx < totalItems-1 {
			m.selectedIdx++
		}
	case tea.KeyEnter:
		return m.activateSelected()
	case tea.KeyHome:
		m.selectedIdx = 0
		m.scrollOffset = 0
		m.scrollbar.SetScrollOffset(0)
		if m.selectedIdx != prevSelected {
			m.invalidateCache()
		}
		return m, nil
	case tea.KeyEnd:
		m.selectedIdx = totalItems - 1
		m.ensureSelectedVisible()
		if m.selectedIdx != prevSelected {
			m.invalidateCache()
		}
		return m, nil
	default:
		return m, nil
	}

	if m.selectedIdx != prevSelected {
		m.invalidateCache()
	}
	m.ensureSelectedVisible()
	return m, nil
}

func (m *model) activateSelected() (layout.Model, tea.Cmd) {
	if m.selectedIdx >= 0 && m.selectedIdx < len(m.sessions) {
		return m, func() tea.Msg {
			return messages.SelectDashboardSessionMsg{
				SessionID: m.sessions[m.selectedIdx].SessionID,
			}
		}
	}
	// "+ New Session" card
	return m, func() tea.Msg {
		return messages.SpawnSessionMsg{}
	}
}

func (m *model) handleMouseClick(msg tea.MouseClickMsg) (layout.Model, tea.Cmd) {
	if msg.Button != tea.MouseLeft {
		return m, nil
	}

	// Check scrollbar first
	sbModel, cmd := m.scrollbar.Update(msg)
	m.scrollbar = sbModel
	if m.scrollbar.IsDragging() {
		m.scrollOffset = m.scrollbar.GetScrollOffset()
		return m, cmd
	}

	// Hit test cards
	idx := m.hitTestCard(msg.X, msg.Y)
	if idx < 0 {
		return m, nil
	}

	now := time.Now()
	isDoubleClick := idx == m.lastClickIdx && now.Sub(m.lastClickTime) <= styles.DoubleClickThreshold

	m.lastClickIdx = idx
	m.lastClickTime = now

	if isDoubleClick {
		// Double-click: activate the session
		m.selectedIdx = idx
		m.invalidateCache()
		return m.activateSelected()
	}

	// Single click: highlight only
	if m.selectedIdx != idx {
		m.selectedIdx = idx
		m.invalidateCache()
	}
	return m, nil
}

func (m *model) handleMouseWheel(msg tea.MouseWheelMsg) (layout.Model, tea.Cmd) {
	switch msg.Button {
	case tea.MouseWheelUp:
		m.scroll(-3)
	case tea.MouseWheelDown:
		m.scroll(3)
	}
	return m, nil
}

func (m *model) handleWheelCoalesced(msg messages.WheelCoalescedMsg) (layout.Model, tea.Cmd) {
	m.scroll(msg.Delta * 3)
	return m, nil
}

func (m *model) scroll(delta int) {
	maxScroll := max(0, m.totalContentHeight()-m.height)
	m.scrollOffset = max(0, min(m.scrollOffset+delta, maxScroll))
	m.scrollbar.SetScrollOffset(m.scrollOffset)
	// Scrolling does NOT invalidate cache — we just extract a different viewport slice.
}

func (m *model) ensureSelectedVisible() {
	if m.height <= 0 || m.cols <= 0 {
		return
	}

	totalItems := len(m.sessions) + 1
	if m.selectedIdx < 0 || m.selectedIdx >= totalItems {
		return
	}

	row := m.selectedIdx / m.cols
	// Title row (2 lines) + card row offset
	cardTop := 2 + row*(cardHeight+cardGapV)
	cardBottom := cardTop + cardHeight

	// Scroll up if card is above viewport
	if cardTop < m.scrollOffset {
		m.scrollOffset = cardTop
	}
	// Scroll down if card is below viewport
	if cardBottom > m.scrollOffset+m.height {
		m.scrollOffset = cardBottom - m.height
	}
	m.scrollOffset = max(0, m.scrollOffset)
	m.scrollbar.SetScrollOffset(m.scrollOffset)
}

func (m *model) hitTestCard(x, y int) int {
	if m.cols <= 0 || m.cardWidth <= 0 {
		return -1
	}

	// Account for scroll offset
	contentY := y + m.scrollOffset

	// Skip title area (2 lines)
	contentY -= 2
	if contentY < 0 {
		return -1
	}

	// Determine row
	rowHeight := cardHeight + cardGapV
	row := contentY / rowHeight
	withinCard := contentY % rowHeight
	if withinCard >= cardHeight {
		return -1 // In the gap between rows
	}

	// Determine column (account for left padding)
	innerX := x - 1 // AppStyle left padding
	colWidth := m.cardWidth + cardGapH
	col := innerX / colWidth
	withinCol := innerX % colWidth
	if withinCol >= m.cardWidth || col >= m.cols {
		return -1 // In the gap between columns or out of bounds
	}

	idx := row*m.cols + col
	totalItems := len(m.sessions) + 1
	if idx >= totalItems {
		return -1
	}
	return idx
}

// ensureCacheBuilt rebuilds the cached lines if the cache is dirty.
// This is the only place where card rendering happens.
func (m *model) ensureCacheBuilt() {
	if !m.cacheDirty && len(m.cachedLines) > 0 {
		return // Cache is valid — skip rendering
	}

	gridW := m.gridWidth()

	var lines []string

	// Title
	titleStyle := styles.BoldStyle.Foreground(styles.White)
	title := titleStyle.Render("Dashboard")
	countStyle := styles.MutedStyle
	count := countStyle.Render(fmt.Sprintf("  %d sessions", len(m.sessions)))
	lines = append(lines, title+count, "") // title + blank line

	// Render cards in grid
	totalItems := len(m.sessions) + 1
	for rowStart := 0; rowStart < totalItems; rowStart += m.cols {
		rowEnd := min(rowStart+m.cols, totalItems)
		var rowCards []string
		for i := rowStart; i < rowEnd; i++ {
			isSelected := i == m.selectedIdx
			if i < len(m.sessions) {
				rowCards = append(rowCards, m.renderSessionCard(m.sessions[i], isSelected))
			} else {
				rowCards = append(rowCards, m.renderNewSessionCard(isSelected))
			}
		}
		// Pad row with empty cards if needed for alignment
		for len(rowCards) < m.cols {
			rowCards = append(rowCards, strings.Repeat(" ", m.cardWidth))
		}
		row := lipgloss.JoinHorizontal(lipgloss.Top, joinWithGap(rowCards, cardGapH)...)
		lines = append(lines, row)
		// Add vertical gap between rows
		if rowStart+m.cols < totalItems {
			lines = append(lines, "")
		}
	}

	// Flatten multi-line card rows into individual lines and truncate to grid width
	content := strings.Join(lines, "\n")
	flat := strings.Split(content, "\n")
	for i, line := range flat {
		if ansi.StringWidth(line) > gridW {
			flat[i] = ansi.Truncate(line, gridW, "")
		}
	}

	m.cachedLines = flat
	m.cacheDirty = false
}

// View renders the dashboard. Uses cached lines when possible — only the
// viewport extraction and scrollbar composition happen on every frame.
func (m *model) View() string {
	if m.width <= 0 || m.height <= 0 {
		return ""
	}

	// Rebuild cache only if dirty (data/selection/size changed)
	m.ensureCacheBuilt()

	// Extract visible viewport from cached lines — O(viewportHeight)
	totalLines := len(m.cachedLines)
	startLine := min(m.scrollOffset, max(0, totalLines-1))
	endLine := min(startLine+m.height, totalLines)

	// Copy visible lines to avoid mutating cache
	visibleLines := make([]string, m.height)
	for i := startLine; i < endLine; i++ {
		visibleLines[i-startLine] = m.cachedLines[i]
	}
	// Remaining lines are already "" (zero value)

	// Update scrollbar
	m.scrollbar.SetDimensions(m.height, m.totalContentHeight())
	m.scrollbar.SetScrollOffset(m.scrollOffset)

	scrollbarView := m.scrollbar.View()
	contentView := strings.Join(visibleLines, "\n")

	if scrollbarView != "" {
		// Build a spacer column (1 space per line, m.height tall)
		spacerLines := make([]string, m.height)
		for i := range spacerLines {
			spacerLines[i] = " "
		}
		spacer := strings.Join(spacerLines, "\n")

		contentView = lipgloss.JoinHorizontal(lipgloss.Top, contentView, spacer, scrollbarView)
	}

	return styles.AppStyle.Height(m.height).Render(contentView)
}

func joinWithGap(items []string, gap int) []string {
	if len(items) <= 1 {
		return items
	}
	result := make([]string, 0, len(items)*2-1)
	spacer := strings.Repeat(" ", gap)
	for i, item := range items {
		if i > 0 {
			result = append(result, spacer)
		}
		result = append(result, item)
	}
	return result
}

func (m *model) renderSessionCard(info supervisor.SessionInfo, selected bool) string {
	cw := m.contentWidth()

	// Border color: selected (cursor) takes priority, otherwise muted.
	// Active session indicated by label, not border — avoids visual confusion.
	borderColor := styles.BorderMuted
	if selected {
		borderColor = styles.Accent
	}
	if info.NeedsAttention {
		borderColor = styles.Warning
	}

	cardStyle := lipgloss.NewStyle().
		Width(m.cardWidth).
		Height(cardHeight).
		Border(lipgloss.RoundedBorder()).
		BorderForeground(borderColor).
		Padding(0, 1)

	// Build card content
	var contentLines []string

	// Line 1: Title with status indicator
	statusIcon := "○"
	statusStyle := styles.MutedStyle
	if info.IsRunning {
		statusIcon = "●"
		statusStyle = styles.SuccessStyle
	}
	if info.NeedsAttention {
		statusIcon = "!"
		statusStyle = styles.WarningStyle
	}

	titleText := truncateStr(info.Title, cw-4)
	titleLine := statusStyle.Render(statusIcon) + " " + styles.BoldStyle.Render(titleText)
	contentLines = append(contentLines, titleLine)

	// Line 2: Working directory (shortened), with active badge if applicable
	workDir := shortenPath(info.WorkingDir, cw-2)
	dirLine := styles.MutedStyle.Render(workDir)
	if info.IsActive {
		badge := styles.InfoStyle.Render(" ◆")
		availWidth := cw - 2 - 2 // 2 for badge
		dirLine = styles.MutedStyle.Render(shortenPath(info.WorkingDir, availWidth)) + badge
	}
	contentLines = append(contentLines,
		dirLine,
		styles.MutedStyle.Render(strings.Repeat("─", cw)), // separator
	)

	// Line 4: Agent name
	agentName := info.AgentName
	if agentName == "" {
		agentName = "root"
	}
	agentLine := styles.SecondaryStyle.Render("Agent: ") + styles.BaseStyle.Render(truncateStr(agentName, cw-8))
	contentLines = append(contentLines, agentLine)

	// Line 5: Cost + tokens
	tokenCount := info.InputTokens + info.OutputTokens
	costStr := formatCost(info.Cost)
	tokensStr := formatTokens(tokenCount)
	statsLine := styles.SecondaryStyle.Render("Cost: ") + costStr +
		styles.MutedStyle.Render("  ") +
		styles.SecondaryStyle.Render("Tok: ") + tokensStr
	contentLines = append(contentLines, truncateStr(statsLine, cw+20)) // allow for ANSI codes

	// Line 6: Messages + duration
	msgStr := fmt.Sprintf("%d msgs", info.MessageCount)
	durStr := formatDuration(info.Duration)
	metaLine := styles.MutedStyle.Render(msgStr)
	if durStr != "" {
		metaLine += styles.MutedStyle.Render("  " + durStr)
	}
	contentLines = append(contentLines, metaLine)

	content := strings.Join(contentLines, "\n")
	return cardStyle.Render(content)
}

func (m *model) renderNewSessionCard(selected bool) string {
	borderColor := styles.BorderMuted
	if selected {
		borderColor = styles.Accent
	}

	cardStyle := lipgloss.NewStyle().
		Width(m.cardWidth).
		Height(cardHeight).
		Border(lipgloss.RoundedBorder()).
		BorderForeground(borderColor).
		Padding(0, 1).
		Align(lipgloss.Center, lipgloss.Center)

	icon := styles.MutedStyle.Render("+")
	label := styles.SecondaryStyle.Render("New Session")
	content := icon + " " + label

	return cardStyle.Render(content)
}

// --- Formatting helpers ---

func truncateStr(s string, maxWidth int) string {
	if maxWidth <= 0 {
		return ""
	}
	// Use lipgloss width for ANSI-aware truncation
	w := lipgloss.Width(s)
	if w <= maxWidth {
		return s
	}
	return lipgloss.NewStyle().MaxWidth(maxWidth).Render(s)
}

func shortenPath(p string, maxWidth int) string {
	if p == "" {
		return ""
	}
	// Try full path first
	if len(p) <= maxWidth {
		return p
	}
	// Use ~/... for home dir
	base := filepath.Base(p)
	parent := filepath.Base(filepath.Dir(p))
	short := parent + "/" + base
	if len(short) <= maxWidth {
		return "…/" + short
	}
	if len(base) <= maxWidth-2 {
		return "…/" + base
	}
	return base[:min(len(base), maxWidth)]
}

func formatCost(cost float64) string {
	if cost == 0 {
		return styles.MutedStyle.Render("—")
	}
	if cost < 0.01 {
		return styles.BaseStyle.Render(fmt.Sprintf("$%.4f", cost))
	}
	return styles.BaseStyle.Render(fmt.Sprintf("$%.2f", cost))
}

func formatTokens(tokens int64) string {
	if tokens == 0 {
		return styles.MutedStyle.Render("—")
	}
	if tokens >= 1_000_000 {
		return styles.BaseStyle.Render(fmt.Sprintf("%.1fM", float64(tokens)/1_000_000))
	}
	if tokens >= 1_000 {
		return styles.BaseStyle.Render(fmt.Sprintf("%.1fk", float64(tokens)/1_000))
	}
	return styles.BaseStyle.Render(fmt.Sprintf("%d", tokens))
}

func formatDuration(d time.Duration) string {
	if d == 0 {
		return ""
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
}
