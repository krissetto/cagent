package dialog

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/cagent/pkg/tui/core"
	"github.com/docker/cagent/pkg/tui/core/layout"
	"github.com/docker/cagent/pkg/tui/messages"
	"github.com/docker/cagent/pkg/tui/styles"
)

type dirEntry struct {
	name string
	path string
}

type workingDirPickerDialog struct {
	BaseDialog
	textInput  textinput.Model
	currentDir string
	entries    []dirEntry
	filtered   []dirEntry
	selected   int
	offset     int
	keyMap     commandPaletteKeyMap
	recentDirs []string
	showRecent bool
	err        error
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
	}

	d.loadDirectory()

	return d
}

func (d *workingDirPickerDialog) loadDirectory() {
	d.entries = nil
	d.filtered = nil
	d.selected = 0
	d.offset = 0
	d.err = nil

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
			}
			return d, nil

		case key.Matches(msg, d.keyMap.Down):
			if d.selected < len(d.filtered)-1 {
				d.selected++
			}
			return d, nil

		case key.Matches(msg, d.keyMap.PageUp):
			d.selected -= d.pageSize()
			if d.selected < 0 {
				d.selected = 0
			}
			return d, nil

		case key.Matches(msg, d.keyMap.PageDown):
			d.selected += d.pageSize()
			if d.selected >= len(d.filtered) {
				d.selected = max(0, len(d.filtered)-1)
			}
			return d, nil

		case key.Matches(msg, d.keyMap.Enter):
			if d.selected >= 0 && d.selected < len(d.filtered) {
				entry := d.filtered[d.selected]

				// Check if it's the "Use this directory" option
				if strings.HasPrefix(entry.name, "→") {
					return d, tea.Sequence(
						core.CmdHandler(CloseDialogMsg{}),
						core.CmdHandler(messages.SpawnSessionMsg{WorkingDir: entry.path}),
					)
				}

				// Navigate into directory
				d.currentDir = entry.path
				d.textInput.SetValue("")
				d.showRecent = false
				d.loadDirectory()
				return d, nil
			}
			return d, nil

		default:
			var cmd tea.Cmd
			d.textInput, cmd = d.textInput.Update(msg)
			d.filterEntries()
			return d, cmd
		}
	}

	return d, nil
}

func (d *workingDirPickerDialog) filterEntries() {
	query := strings.ToLower(strings.TrimSpace(d.textInput.Value()))
	if query == "" {
		d.filtered = d.entries
		d.selected = 0
		d.offset = 0
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
		d.selected = 0
	}
	d.offset = 0
}

func (d *workingDirPickerDialog) dialogSize() (dialogWidth, maxHeight, contentWidth int) {
	dialogWidth = max(min(d.Width()*80/100, 80), 60)
	maxHeight = min(d.Height()*70/100, 30)
	contentWidth = dialogWidth - 6
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

	var entryLines []string

	// Show recent directories if available and on initial view
	if d.showRecent && len(d.recentDirs) > 0 && d.textInput.Value() == "" {
		entryLines = append(entryLines, styles.MutedStyle.Render("Recent directories:"))
		for i, dir := range d.recentDirs {
			if i >= 5 {
				break
			}
			displayPath := dir
			if len(displayPath) > contentWidth-6 {
				displayPath = "…" + displayPath[len(displayPath)-(contentWidth-7):]
			}
			entryLines = append(entryLines, styles.PaletteUnselectedDescStyle.Render("  "+displayPath))
		}
		entryLines = append(entryLines, "")
	}

	maxItems := maxHeight - 12 - len(entryLines)

	// Adjust offset to keep selected item visible
	if d.selected < d.offset {
		d.offset = d.selected
	} else if d.selected >= d.offset+maxItems {
		d.offset = d.selected - maxItems + 1
	}

	// Render visible items based on offset
	visibleEnd := min(d.offset+maxItems, len(d.filtered))
	for i := d.offset; i < visibleEnd; i++ {
		entryLines = append(entryLines, d.renderEntry(d.filtered[i], i == d.selected, contentWidth))
	}

	// Show indicator if there are more items
	if visibleEnd < len(d.filtered) {
		entryLines = append(entryLines, styles.MutedStyle.Render(fmt.Sprintf("  … and %d more", len(d.filtered)-visibleEnd)))
	}

	if d.err != nil {
		entryLines = append(entryLines, "", styles.ErrorStyle.
			Align(lipgloss.Center).
			Width(contentWidth).
			Render(d.err.Error()))
	} else if len(d.filtered) == 0 {
		entryLines = append(entryLines, "", styles.DialogContentStyle.
			Italic(true).
			Align(lipgloss.Center).
			Width(contentWidth).
			Render("No directories found"))
	}

	content := NewContent(contentWidth).
		AddTitle("Select Working Directory").
		AddSpace().
		AddContent(dirLine).
		AddContent(d.textInput.View()).
		AddSeparator().
		AddContent(strings.Join(entryLines, "\n")).
		AddSpace().
		AddHelpKeys("↑/↓", "navigate", "enter", "select/enter dir", "esc", "cancel").
		Build()

	return styles.DialogStyle.Width(dialogWidth).Render(content)
}

func (d *workingDirPickerDialog) pageSize() int {
	_, maxHeight, _ := d.dialogSize()
	return max(1, maxHeight-12)
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
