// Package tabbar provides a horizontal tab bar for multi-session TUI support.
package tabbar

import (
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/cagent/pkg/tui/core"
	"github.com/docker/cagent/pkg/tui/messages"
	"github.com/docker/cagent/pkg/tui/styles"
)

const (
	// scrollArrowWidth is the visual width of a scroll indicator ("◀ " or " ▶").
	scrollArrowWidth = 2
	// maxTitleLen is the maximum display length for a tab title.
	maxTitleLen = 20
	// separatorChar is the character between tabs.
	separatorChar = "│"
)

// renderedTab holds a pre-rendered tab view and its measured visual width.
type renderedTab struct {
	view  string
	width int
}

// clickZone records where a clickable element is on the tab bar.
type clickZone struct {
	startX, endX int
	tabIdx       int // index into tabs (-1 for non-tab zones)
	isPlus       bool
	isScrollLeft bool
	isScrollRight bool
}

// TabBar renders a horizontal bar of session tabs with click and keyboard support.
type TabBar struct {
	tabs      []messages.TabInfo
	activeIdx int
	width     int
	keyMap    KeyMap

	// Scroll state — index of the first visible tab.
	scrollOffset int

	// Click zones computed during View for accurate hit detection.
	zones []clickZone
}

// KeyMap defines key bindings for the tab bar.
type KeyMap struct {
	NewTab   key.Binding
	NextTab  key.Binding
	PrevTab  key.Binding
	CloseTab key.Binding
}

// DefaultKeyMap returns the default tab bar key bindings.
func DefaultKeyMap() KeyMap {
	return KeyMap{
		NewTab: key.NewBinding(
			key.WithKeys("f2"),
			key.WithHelp("F2", "new tab"),
		),
		NextTab: key.NewBinding(
			key.WithKeys("ctrl+tab", "ctrl+pgdown"),
			key.WithHelp("Ctrl+Tab", "next tab"),
		),
		PrevTab: key.NewBinding(
			key.WithKeys("ctrl+shift+tab", "ctrl+pgup"),
			key.WithHelp("Ctrl+Shift+Tab", "prev tab"),
		),
		CloseTab: key.NewBinding(
			key.WithKeys("ctrl+w"),
			key.WithHelp("Ctrl+W", "close tab"),
		),
	}
}

// New creates a new tab bar.
func New() *TabBar {
	return &TabBar{
		keyMap: DefaultKeyMap(),
	}
}

// SetWidth sets the available width for the tab bar.
func (t *TabBar) SetWidth(width int) {
	t.width = width
}

// SetTabs updates the list of tabs and active index.
func (t *TabBar) SetTabs(tabs []messages.TabInfo, activeIdx int) {
	t.tabs = tabs
	t.activeIdx = activeIdx
	t.clampScroll()
}

// Height returns the height of the tab bar (always 1).
func (t *TabBar) Height() int {
	return 1
}

// Bindings returns the key bindings for the tab bar.
func (t *TabBar) Bindings() []key.Binding {
	return []key.Binding{
		t.keyMap.NewTab,
		t.keyMap.NextTab,
		t.keyMap.PrevTab,
		t.keyMap.CloseTab,
	}
}

// Update handles messages and returns commands.
func (t *TabBar) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, t.keyMap.NewTab):
			return core.CmdHandler(messages.SpawnSessionMsg{})

		case key.Matches(msg, t.keyMap.NextTab):
			if len(t.tabs) <= 1 {
				return nil
			}
			nextIdx := (t.activeIdx + 1) % len(t.tabs)
			return core.CmdHandler(messages.SwitchTabMsg{SessionID: t.tabs[nextIdx].SessionID})

		case key.Matches(msg, t.keyMap.PrevTab):
			if len(t.tabs) <= 1 {
				return nil
			}
			prevIdx := t.activeIdx - 1
			if prevIdx < 0 {
				prevIdx = len(t.tabs) - 1
			}
			return core.CmdHandler(messages.SwitchTabMsg{SessionID: t.tabs[prevIdx].SessionID})

		case key.Matches(msg, t.keyMap.CloseTab):
			if len(t.tabs) == 0 {
				return nil
			}
			return core.CmdHandler(messages.CloseTabMsg{SessionID: t.tabs[t.activeIdx].SessionID})
		}

	case tea.MouseClickMsg:
		if msg.Y == 0 {
			return t.handleClick(msg.X)
		}
	}

	return nil
}

// handleClick uses the click zones computed during the last View() call
// for pixel-accurate hit detection.
func (t *TabBar) handleClick(x int) tea.Cmd {
	for _, z := range t.zones {
		if x >= z.startX && x < z.endX {
			switch {
			case z.isScrollLeft:
				t.scrollOffset = max(0, t.scrollOffset-1)
				return nil
			case z.isScrollRight:
				t.scrollOffset = min(len(t.tabs)-1, t.scrollOffset+1)
				return nil
			case z.isPlus:
				return core.CmdHandler(messages.SpawnSessionMsg{})
			case z.tabIdx >= 0 && z.tabIdx != t.activeIdx:
				return core.CmdHandler(messages.SwitchTabMsg{SessionID: t.tabs[z.tabIdx].SessionID})
			}
			return nil
		}
	}
	return nil
}

// View renders the tab bar.
func (t *TabBar) View() string {
	if len(t.tabs) == 0 {
		return ""
	}

	// Reset click zones
	t.zones = t.zones[:0]

	// Style definitions
	activeStyle := lipgloss.NewStyle().
		Background(styles.BackgroundAlt).
		Foreground(styles.TextPrimary).
		Bold(true).
		Padding(0, 1)

	inactiveStyle := lipgloss.NewStyle().
		Foreground(styles.TextMuted).
		Padding(0, 1)

	runningIndicator := lipgloss.NewStyle().
		Foreground(styles.Success).
		Render("● ")

	attentionIndicator := lipgloss.NewStyle().
		Foreground(styles.Warning).
		Bold(true).
		Render(" !")

	sepStyle := lipgloss.NewStyle().Foreground(styles.BorderMuted)
	separator := sepStyle.Render(separatorChar)
	sepWidth := lipgloss.Width(separator)

	plusStyle := lipgloss.NewStyle().Foreground(styles.TextMuted)
	plusView := plusStyle.Render(" + ")
	plusWidth := lipgloss.Width(plusView)

	arrowStyle := lipgloss.NewStyle().Foreground(styles.TextMuted)

	// Pre-render every tab so we can measure accurate widths.
	allTabs := make([]renderedTab, len(t.tabs))
	totalWidth := 0
	for i, tab := range t.tabs {
		v := t.renderSingleTab(tab, activeStyle, inactiveStyle, runningIndicator, attentionIndicator)
		w := lipgloss.Width(v)
		allTabs[i] = renderedTab{view: v, width: w}
		totalWidth += w
		if i < len(t.tabs)-1 {
			totalWidth += sepWidth
		}
	}
	// Account for separator before "+" and the "+" itself
	totalWidth += sepWidth + plusWidth

	availWidth := t.width
	if availWidth <= 0 {
		availWidth = 200
	}

	needsScroll := totalWidth > availWidth

	// Ensure active tab is visible within the scroll window.
	if needsScroll {
		t.ensureActiveVisible(allTabs, sepWidth, plusWidth, availWidth)
	} else {
		t.scrollOffset = 0
	}

	// Build the visible portion of the tab line.
	var cursor int // tracks X position for click zones

	hasLeftArrow := needsScroll && t.scrollOffset > 0

	if hasLeftArrow {
		arrow := arrowStyle.Render("◀ ")
		t.zones = append(t.zones, clickZone{startX: cursor, endX: cursor + scrollArrowWidth, isScrollLeft: true})
		cursor += scrollArrowWidth
		// We'll prepend this later; for now just track the width
		_ = arrow
	}

	// Collect visible parts and determine last visible index.
	type part struct {
		text  string
		width int
	}
	var parts []part

	if hasLeftArrow {
		parts = append(parts, part{text: arrowStyle.Render("◀ "), width: scrollArrowWidth})
	}

	lastVisibleIdx := t.scrollOffset - 1
	usedWidth := cursor
	for i := t.scrollOffset; i < len(allTabs); i++ {
		tabW := allTabs[i].width

		// Reserve space for: possible right arrow + separator + "+"
		rightReserve := sepWidth + plusWidth
		if needsScroll && i < len(allTabs)-1 {
			rightReserve += scrollArrowWidth // for " ▶"
		}

		if usedWidth+tabW+rightReserve > availWidth && i > t.scrollOffset {
			break
		}

		// Add separator before tab (except for the first visible tab)
		if i > t.scrollOffset {
			parts = append(parts, part{text: separator, width: sepWidth})
			usedWidth += sepWidth
		}

		// Record click zone for this tab
		t.zones = append(t.zones, clickZone{startX: usedWidth, endX: usedWidth + tabW, tabIdx: i})
		parts = append(parts, part{text: allTabs[i].view, width: tabW})
		usedWidth += tabW
		lastVisibleIdx = i
	}

	// Right scroll indicator
	hasRightArrow := needsScroll && lastVisibleIdx < len(allTabs)-1
	if hasRightArrow {
		arrow := arrowStyle.Render(" ▶")
		t.zones = append(t.zones, clickZone{startX: usedWidth, endX: usedWidth + scrollArrowWidth, isScrollRight: true})
		parts = append(parts, part{text: arrow, width: scrollArrowWidth})
		usedWidth += scrollArrowWidth
	}

	// Separator + "+" button (always shown)
	parts = append(parts, part{text: separator, width: sepWidth})
	usedWidth += sepWidth
	t.zones = append(t.zones, clickZone{startX: usedWidth, endX: usedWidth + plusWidth, isPlus: true})
	parts = append(parts, part{text: plusView, width: plusWidth})
	usedWidth += plusWidth

	// Join all parts
	strs := make([]string, len(parts))
	for i, p := range parts {
		strs[i] = p.text
	}
	tabLine := lipgloss.JoinHorizontal(lipgloss.Center, strs...)

	// Fill remaining width with background
	lineWidth := lipgloss.Width(tabLine)
	if lineWidth < availWidth {
		filler := lipgloss.NewStyle().
			Width(availWidth - lineWidth).
			Render("")
		tabLine = lipgloss.JoinHorizontal(lipgloss.Left, tabLine, filler)
	}

	return tabLine
}

// renderSingleTab renders one tab with appropriate styling.
func (t *TabBar) renderSingleTab(
	tab messages.TabInfo,
	activeStyle, inactiveStyle lipgloss.Style,
	runningIndicator, attentionIndicator string,
) string {
	title := tab.Title
	if title == "" {
		title = "New Session"
	}
	if len(title) > maxTitleLen {
		title = title[:maxTitleLen-1] + "…"
	}

	var content string
	if tab.IsRunning {
		content += runningIndicator
	}
	content += title
	if tab.NeedsAttention {
		content += attentionIndicator
	}

	if tab.IsActive {
		return activeStyle.Render(content)
	}
	return inactiveStyle.Render(content)
}

// ensureActiveVisible adjusts scrollOffset so the active tab is within the
// visible window, accounting for arrows and the "+" button.
func (t *TabBar) ensureActiveVisible(tabs []renderedTab, sepWidth, plusWidth, availWidth int) {
	// Scroll left if active is before the visible window
	if t.activeIdx < t.scrollOffset {
		t.scrollOffset = t.activeIdx
	}

	// Scroll right if active doesn't fit
	for t.scrollOffset < t.activeIdx {
		usedWidth := 0
		if t.scrollOffset > 0 {
			usedWidth += scrollArrowWidth
		}

		fits := false
		for i := t.scrollOffset; i < len(tabs); i++ {
			if i > t.scrollOffset {
				usedWidth += sepWidth
			}
			usedWidth += tabs[i].width

			if i == t.activeIdx {
				// Check if there's room for: possible right arrow + separator + "+"
				rightReserve := sepWidth + plusWidth
				if i < len(tabs)-1 {
					rightReserve += scrollArrowWidth
				}
				if usedWidth+rightReserve <= availWidth {
					fits = true
				}
				break
			}
		}

		if fits {
			break
		}
		t.scrollOffset++
	}
}

// clampScroll ensures scrollOffset is within valid bounds.
func (t *TabBar) clampScroll() {
	if t.scrollOffset >= len(t.tabs) {
		t.scrollOffset = max(0, len(t.tabs)-1)
	}
	if t.activeIdx < t.scrollOffset {
		t.scrollOffset = t.activeIdx
	}
}
