package dialog

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/cagent/pkg/tui/components/scrollbar"
	"github.com/docker/cagent/pkg/tui/core"
	"github.com/docker/cagent/pkg/tui/core/layout"
	"github.com/docker/cagent/pkg/tui/messages"
	"github.com/docker/cagent/pkg/tui/styles"
)

type dirEntry struct {
	name string
	path string
}

// Working directory picker dialog dimension constants
const (
	// dirPickerWidthPercent is the percentage of screen width to use for the dialog
	dirPickerWidthPercent = 80
	// dirPickerMinWidth is the minimum width of the dialog
	dirPickerMinWidth = 50
	// dirPickerMaxWidth is the maximum width of the dialog
	dirPickerMaxWidth = 100
	// dirPickerHeightPercent is the percentage of screen height to use for the dialog
	dirPickerHeightPercent = 70
	// dirPickerMaxHeight is the maximum height of the dialog
	dirPickerMaxHeight = 150

	// dirPickerDialogPadding is the horizontal padding inside the dialog border (2 on each side + border)
	dirPickerDialogPadding = 6

	// dirPickerListVerticalOverhead is the number of rows used by dialog chrome:
	// title(1) + space(1) + dir line(1) + input(1) + separator(1) + space at bottom(1) + help keys(1) + borders/padding(2) = 9
	dirPickerListVerticalOverhead = 9

	// dirPickerListStartOffset is the Y offset from dialog top to where the directory list starts:
	// border(1) + padding(1) + title(1) + space(1) + dir line(1) + input(1) + separator(1) = 7
	dirPickerListStartOffset = 7

	// dirPickerScrollbarYOffset is the Y offset from dialog top to where the scrollbar starts.
	// Must match dirPickerListStartOffset since they are joined horizontally.
	dirPickerScrollbarYOffset = dirPickerListStartOffset

	// dirPickerScrollbarXInset is the inset from dialog right edge for the scrollbar
	dirPickerScrollbarXInset = 3

	// dirPickerScrollbarGap is the space between content and the scrollbar
	dirPickerScrollbarGap = 1

	// dirPickerMaxRecentDirs is the maximum number of recent directories to show
	dirPickerMaxRecentDirs = 5

	// recentSeparatorLabel is the text for the recent directories section separator
	recentSeparatorLabel = "── Recent directories "
)

type workingDirPickerDialog struct {
	BaseDialog
	textInput        textinput.Model
	currentDir       string
	entries          []dirEntry
	filtered         []dirEntry
	selected         int
	keyMap           commandPaletteKeyMap
	recentDirs       []string
	showRecent       bool
	err              error
	scrollbar        *scrollbar.Model
	needsScrollToSel bool // true when keyboard nav requires scrolling to selection

	// Double-click detection
	lastClickTime  time.Time
	lastClickIndex int
}

// NewWorkingDirPickerDialog creates a new working directory picker dialog.
// recentDirs provides a list of recently used directories to show at the top.
func NewWorkingDirPickerDialog(recentDirs []string) Dialog {
	ti := textinput.New()
	ti.Placeholder = "Type to filter directories…"
	ti.Focus()
	ti.CharLimit = 256
	ti.SetWidth(50)

	cwd, err := os.Getwd()
	if err != nil {
		cwd = "/"
	}

	d := &workingDirPickerDialog{
		textInput:  ti,
		currentDir: cwd,
		recentDirs: recentDirs,
		showRecent: len(recentDirs) > 0,
		keyMap:     defaultCommandPaletteKeyMap(),
		scrollbar:  scrollbar.New(),
	}

	d.loadDirectory()

	return d
}

func (d *workingDirPickerDialog) loadDirectory() {
	d.entries = nil
	d.filtered = nil
	d.selected = 0
	d.err = nil
	d.scrollbar.SetScrollOffset(0)

	// Add "Use this directory" option at the top
	d.entries = append(d.entries, dirEntry{
		name: "→ Use this directory",
		path: d.currentDir,
	})

	// Add parent directory entry if not at root
	if d.currentDir != "/" {
		d.entries = append(d.entries, dirEntry{
			name: "..",
			path: filepath.Dir(d.currentDir),
		})
	}

	dirEntries, err := os.ReadDir(d.currentDir)
	if err != nil {
		d.err = err
		d.filtered = d.entries
		return
	}

	// Only add directories
	for _, entry := range dirEntries {
		// Skip hidden directories
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}

		if entry.IsDir() {
			fullPath := filepath.Join(d.currentDir, entry.Name())
			d.entries = append(d.entries, dirEntry{
				name: entry.Name() + "/",
				path: fullPath,
			})
		}
	}

	d.filtered = d.entries
}

func (d *workingDirPickerDialog) Init() tea.Cmd {
	return textinput.Blink
}

func (d *workingDirPickerDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		cmd := d.SetSize(msg.Width, msg.Height)
		return d, cmd

	case tea.PasteMsg:
		var cmd tea.Cmd
		d.textInput, cmd = d.textInput.Update(msg)
		d.filterEntries()
		return d, cmd

	case tea.MouseClickMsg:
		return d.handleMouseClick(msg)

	case tea.MouseMotionMsg:
		return d.handleMouseMotion(msg)

	case tea.MouseReleaseMsg:
		return d.handleMouseRelease(msg)

	case tea.MouseWheelMsg:
		return d.handleMouseWheel(msg)

	case messages.WheelCoalescedMsg:
		return d.handleWheelCoalesced(msg)

	case tea.KeyPressMsg:
		if cmd := HandleQuit(msg); cmd != nil {
			return d, cmd
		}

		switch {
		case key.Matches(msg, d.keyMap.Escape):
			return d, core.CmdHandler(CloseDialogMsg{})

		case key.Matches(msg, d.keyMap.Up):
			if d.selected > 0 {
				d.selected--
				d.needsScrollToSel = true
			}
			return d, nil

		case key.Matches(msg, d.keyMap.Down):
			if d.selected < len(d.filtered)-1 {
				d.selected++
				d.needsScrollToSel = true
			}
			return d, nil

		case key.Matches(msg, d.keyMap.PageUp):
			d.selected -= d.pageSize()
			if d.selected < 0 {
				d.selected = 0
			}
			d.needsScrollToSel = true
			return d, nil

		case key.Matches(msg, d.keyMap.PageDown):
			d.selected += d.pageSize()
			if d.selected >= len(d.filtered) {
				d.selected = max(0, len(d.filtered)-1)
			}
			d.needsScrollToSel = true
			return d, nil

		case key.Matches(msg, d.keyMap.Enter):
			cmd := d.handleSelection()
			return d, cmd

		default:
			var cmd tea.Cmd
			d.textInput, cmd = d.textInput.Update(msg)
			d.filterEntries()
			return d, cmd
		}
	}

	return d, nil
}

// handleMouseClick handles mouse click events on the dialog.
func (d *workingDirPickerDialog) handleMouseClick(msg tea.MouseClickMsg) (layout.Model, tea.Cmd) {
	// Check if click is on the scrollbar
	if d.isMouseOnScrollbar(msg.X, msg.Y) {
		sb, cmd := d.scrollbar.Update(msg)
		d.scrollbar = sb
		return d, cmd
	}

	// Check if click is on an entry in the list
	if msg.Button == tea.MouseLeft {
		if entryIdx := d.mouseYToEntryIndex(msg.Y); entryIdx >= 0 {
			now := time.Now()

			// Check for double-click: same index within threshold
			if entryIdx == d.lastClickIndex && now.Sub(d.lastClickTime) < styles.DoubleClickThreshold {
				// Double-click: confirm selection
				d.selected = entryIdx
				d.lastClickTime = time.Time{} // Reset to prevent triple-click
				cmd := d.handleSelection()
				return d, cmd
			}

			// Single click: just highlight
			d.selected = entryIdx
			d.lastClickTime = now
			d.lastClickIndex = entryIdx
		}
	}
	return d, nil
}

// handleMouseMotion handles mouse drag events (for scrollbar dragging).
func (d *workingDirPickerDialog) handleMouseMotion(msg tea.MouseMotionMsg) (layout.Model, tea.Cmd) {
	if d.scrollbar.IsDragging() {
		sb, cmd := d.scrollbar.Update(msg)
		d.scrollbar = sb
		return d, cmd
	}
	return d, nil
}

// handleMouseRelease handles mouse button release events.
func (d *workingDirPickerDialog) handleMouseRelease(msg tea.MouseReleaseMsg) (layout.Model, tea.Cmd) {
	if d.scrollbar.IsDragging() {
		sb, cmd := d.scrollbar.Update(msg)
		d.scrollbar = sb
		return d, cmd
	}
	return d, nil
}

// handleMouseWheel handles mouse wheel scrolling inside the dialog.
func (d *workingDirPickerDialog) handleMouseWheel(msg tea.MouseWheelMsg) (layout.Model, tea.Cmd) {
	// Only scroll if mouse is inside the dialog
	if !d.isMouseInDialog(msg.X, msg.Y) {
		return d, nil
	}

	switch msg.Button {
	case tea.MouseWheelUp:
		d.scrollbar.ScrollUp()
		d.scrollbar.ScrollUp() // Scroll 2 lines at a time
	case tea.MouseWheelDown:
		d.scrollbar.ScrollDown()
		d.scrollbar.ScrollDown() // Scroll 2 lines at a time
	}
	return d, nil
}

// handleWheelCoalesced handles coalesced wheel events (aggregated deltas).
// The background model forwards WheelCoalescedMsg to dialogs, so we convert
// the delta back into individual scroll operations.
func (d *workingDirPickerDialog) handleWheelCoalesced(msg messages.WheelCoalescedMsg) (layout.Model, tea.Cmd) {
	if !d.isMouseInDialog(msg.X, msg.Y) {
		return d, nil
	}

	steps := msg.Delta
	if steps < 0 {
		steps = -steps
		for range steps {
			d.scrollbar.ScrollUp()
		}
	} else {
		for range steps {
			d.scrollbar.ScrollDown()
		}
	}
	return d, nil
}

// isMouseInDialog checks if the mouse position is inside the dialog bounds.
func (d *workingDirPickerDialog) isMouseInDialog(x, y int) bool {
	dialogRow, dialogCol := d.Position()
	dialogWidth, maxHeight, _ := d.dialogSize()

	return x >= dialogCol && x < dialogCol+dialogWidth &&
		y >= dialogRow && y < dialogRow+maxHeight
}

// isMouseOnScrollbar checks if the mouse position is on the scrollbar.
func (d *workingDirPickerDialog) isMouseOnScrollbar(x, y int) bool {
	dialogWidth, maxHeight, _ := d.dialogSize()
	maxItems := maxHeight - dirPickerListVerticalOverhead

	// Total lines includes both the recent section and filtered entries
	totalLines := d.recentSectionLineCount() + len(d.filtered)
	if totalLines <= maxItems {
		return false // No scrollbar when content fits
	}

	dialogRow, dialogCol := d.Position()
	scrollbarX := dialogCol + dialogWidth - dirPickerScrollbarXInset - scrollbar.Width
	scrollbarY := dialogRow + dirPickerScrollbarYOffset

	return x >= scrollbarX && x < scrollbarX+scrollbar.Width &&
		y >= scrollbarY && y < scrollbarY+maxItems
}

// mouseYToEntryIndex converts a mouse Y position to an entry index.
// Returns -1 if the position is not on an entry (e.g., on a separator or outside the list).
func (d *workingDirPickerDialog) mouseYToEntryIndex(y int) int {
	dialogRow, _ := d.Position()
	_, maxHeight, _ := d.dialogSize()
	maxItems := maxHeight - dirPickerListVerticalOverhead

	listStartY := dialogRow + dirPickerListStartOffset
	listEndY := listStartY + maxItems

	// Check if Y is within the directory list area
	if y < listStartY || y >= listEndY {
		return -1
	}

	// Calculate which line in the visible area was clicked
	lineInView := y - listStartY
	scrollOffset := d.scrollbar.GetScrollOffset()

	// Calculate the actual line index in allEntryLines
	actualLine := scrollOffset + lineInView

	// Map the line back to an entry index, accounting for separators
	return d.lineToEntryIndex(actualLine)
}

// recentSectionLineCount returns the number of non-entry lines prepended to allEntryLines
// by the recent directories section (separator + recent entries + blank line).
// Returns 0 when the recent section is not shown.
func (d *workingDirPickerDialog) recentSectionLineCount() int {
	if !d.showRecent || len(d.recentDirs) == 0 || d.textInput.Value() != "" {
		return 0
	}
	recentCount := min(len(d.recentDirs), dirPickerMaxRecentDirs)
	return 1 + recentCount + 1 // separator + recent entries + blank line
}

// lineToEntryIndex converts a line index (in allEntryLines) to an entry index.
// Returns -1 if the line falls on a non-entry line (recent section) or is out of range.
func (d *workingDirPickerDialog) lineToEntryIndex(lineIdx int) int {
	// Skip past the recent directories section lines
	extraLines := d.recentSectionLineCount()
	if lineIdx < extraLines {
		return -1 // Click landed on the recent section, not an entry
	}

	entryIdx := lineIdx - extraLines
	if entryIdx >= 0 && entryIdx < len(d.filtered) {
		return entryIdx
	}

	return -1 // Line index out of range
}

func (d *workingDirPickerDialog) handleSelection() tea.Cmd {
	if d.selected < 0 || d.selected >= len(d.filtered) {
		return nil
	}

	entry := d.filtered[d.selected]

	// Check if it's the "Use this directory" option
	if strings.HasPrefix(entry.name, "→") {
		return tea.Sequence(
			core.CmdHandler(CloseDialogMsg{}),
			core.CmdHandler(messages.SpawnSessionMsg{WorkingDir: entry.path}),
		)
	}

	// Navigate into directory
	d.currentDir = entry.path
	d.textInput.SetValue("")
	d.showRecent = false
	d.loadDirectory()
	return nil
}

func (d *workingDirPickerDialog) filterEntries() {
	query := strings.ToLower(strings.TrimSpace(d.textInput.Value()))
	if query == "" {
		d.filtered = d.entries
		d.selected = 0
		d.scrollbar.SetScrollOffset(0)
		return
	}

	d.filtered = nil
	for _, entry := range d.entries {
		// Always include special entries in filter results
		if strings.HasPrefix(entry.name, "→") || entry.name == ".." {
			d.filtered = append(d.filtered, entry)
			continue
		}

		if strings.Contains(strings.ToLower(entry.name), query) {
			d.filtered = append(d.filtered, entry)
		}
	}

	if d.selected >= len(d.filtered) {
		d.selected = max(0, len(d.filtered)-1)
	}
	// Reset scrollbar when filtering
	d.scrollbar.SetScrollOffset(0)
}

func (d *workingDirPickerDialog) dialogSize() (dialogWidth, maxHeight, contentWidth int) {
	dialogWidth = max(min(d.Width()*dirPickerWidthPercent/100, dirPickerMaxWidth), dirPickerMinWidth)
	maxHeight = min(d.Height()*dirPickerHeightPercent/100, dirPickerMaxHeight)
	contentWidth = dialogWidth - dirPickerDialogPadding - scrollbar.Width - dirPickerScrollbarGap
	return dialogWidth, maxHeight, contentWidth
}

func (d *workingDirPickerDialog) View() string {
	dialogWidth, maxHeight, contentWidth := d.dialogSize()

	d.textInput.SetWidth(contentWidth)

	// Show current directory
	displayDir := d.currentDir
	if len(displayDir) > contentWidth-4 {
		displayDir = "…" + displayDir[len(displayDir)-(contentWidth-5):]
	}
	dirLine := styles.MutedStyle.Render("📁 " + displayDir)

	maxItems := maxHeight - dirPickerListVerticalOverhead

	// Build all entry lines first to calculate total height
	var allEntryLines []string

	// Show recent directories if available and on initial view
	if d.showRecent && len(d.recentDirs) > 0 && d.textInput.Value() == "" {
		separatorLine := styles.MutedStyle.Render(recentSeparatorLabel + strings.Repeat("─", max(0, contentWidth-len(recentSeparatorLabel)-2)))
		allEntryLines = append(allEntryLines, separatorLine)

		for i, dir := range d.recentDirs {
			if i >= dirPickerMaxRecentDirs {
				break
			}
			displayPath := dir
			if len(displayPath) > contentWidth-6 {
				displayPath = "…" + displayPath[len(displayPath)-(contentWidth-7):]
			}
			allEntryLines = append(allEntryLines, styles.PaletteUnselectedDescStyle.Render("  "+displayPath))
		}
		allEntryLines = append(allEntryLines, "")
	}

	for i, entry := range d.filtered {
		allEntryLines = append(allEntryLines, d.renderEntry(entry, i == d.selected, contentWidth))
	}

	totalLines := len(allEntryLines)
	visibleLines := maxItems

	// Update scrollbar dimensions
	d.scrollbar.SetDimensions(visibleLines, totalLines)

	// Only auto-scroll to selection when keyboard navigation occurred
	if d.needsScrollToSel {
		selectedLine := d.findSelectedLine(allEntryLines)
		scrollOffset := d.scrollbar.GetScrollOffset()
		if selectedLine < scrollOffset {
			d.scrollbar.SetScrollOffset(selectedLine)
		} else if selectedLine >= scrollOffset+visibleLines {
			d.scrollbar.SetScrollOffset(selectedLine - visibleLines + 1)
		}
		d.needsScrollToSel = false
	}

	// Slice visible lines based on scroll offset
	scrollOffset := d.scrollbar.GetScrollOffset()
	visibleEnd := min(scrollOffset+visibleLines, totalLines)
	visibleEntryLines := allEntryLines[scrollOffset:visibleEnd]

	// Pad with empty lines if content is shorter than visible area
	for len(visibleEntryLines) < visibleLines {
		visibleEntryLines = append(visibleEntryLines, "")
	}

	// Handle empty state
	if len(d.filtered) == 0 {
		visibleEntryLines = []string{"", styles.DialogContentStyle.
			Italic(true).
			Align(lipgloss.Center).
			Width(contentWidth).
			Render("No directories found")}
		for len(visibleEntryLines) < visibleLines {
			visibleEntryLines = append(visibleEntryLines, "")
		}
	}

	if d.err != nil {
		visibleEntryLines = append(visibleEntryLines, "", styles.ErrorStyle.
			Align(lipgloss.Center).
			Width(contentWidth).
			Render(d.err.Error()))
	}

	// Build entry list with fixed width to keep scrollbar position stable
	entryListStyle := lipgloss.NewStyle().Width(contentWidth)
	var fixedWidthLines []string
	for _, line := range visibleEntryLines {
		fixedWidthLines = append(fixedWidthLines, entryListStyle.Render(line))
	}
	entryListContent := strings.Join(fixedWidthLines, "\n")

	// Set scrollbar position for mouse hit testing
	dialogRow, dialogCol := d.Position()
	scrollbarX := dialogCol + dialogWidth - dirPickerScrollbarXInset - scrollbar.Width
	scrollbarY := dialogRow + dirPickerScrollbarYOffset
	d.scrollbar.SetPosition(scrollbarX, scrollbarY)

	// Get scrollbar view
	scrollbarView := d.scrollbar.View()

	// Combine content with scrollbar (gap between content and scrollbar)
	// Always include the gap and scrollbar space to maintain consistent layout
	var scrollableContent string
	gap := strings.Repeat(" ", dirPickerScrollbarGap)
	if scrollbarView != "" {
		scrollableContent = lipgloss.JoinHorizontal(lipgloss.Top, entryListContent, gap, scrollbarView)
	} else {
		// No scrollbar needed, but still pad to maintain consistent width
		scrollbarPlaceholder := strings.Repeat(" ", scrollbar.Width)
		scrollableContent = lipgloss.JoinHorizontal(lipgloss.Top, entryListContent, gap, scrollbarPlaceholder)
	}

	content := NewContent(contentWidth+dirPickerScrollbarGap+scrollbar.Width).
		AddTitle("New Session: Select Working Directory").
		AddSpace().
		AddContent(dirLine).
		AddContent(d.textInput.View()).
		AddSeparator().
		AddContent(scrollableContent).
		AddSpace().
		AddHelpKeys("↑/↓", "navigate", "enter", "select/enter dir", "esc", "cancel").
		Build()

	return styles.DialogStyle.Width(dialogWidth).Render(content)
}

// findSelectedLine returns the line index in allEntryLines that corresponds to the selected entry.
// This accounts for any non-entry lines (recent dirs section) that may be inserted.
func (d *workingDirPickerDialog) findSelectedLine(_ []string) int {
	if d.selected < 0 || d.selected >= len(d.filtered) {
		return 0
	}

	return d.recentSectionLineCount() + d.selected
}

func (d *workingDirPickerDialog) pageSize() int {
	_, maxHeight, _ := d.dialogSize()
	return max(1, maxHeight-dirPickerListVerticalOverhead)
}

func (d *workingDirPickerDialog) renderEntry(entry dirEntry, selected bool, maxWidth int) string {
	nameStyle := styles.PaletteUnselectedActionStyle
	if selected {
		nameStyle = styles.PaletteSelectedActionStyle
	}

	var icon string
	if strings.HasPrefix(entry.name, "→") {
		icon = "✓ "
	} else {
		icon = "📁 "
	}

	name := entry.name
	if strings.HasPrefix(name, "→") {
		name = "Use this directory"
	}

	maxNameLen := maxWidth - 6
	if len(name) > maxNameLen {
		name = name[:maxNameLen-1] + "…"
	}

	return nameStyle.Render(icon + name)
}

func (d *workingDirPickerDialog) Position() (row, col int) {
	dialogWidth, maxHeight, _ := d.dialogSize()
	return CenterPosition(d.Width(), d.Height(), dialogWidth, maxHeight)
}
