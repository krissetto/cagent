package dialog

import (
	"fmt"
	"sort"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/atotto/clipboard"

	"github.com/docker/cagent/pkg/chat"
	"github.com/docker/cagent/pkg/session"
	"github.com/docker/cagent/pkg/tui/components/notification"
	"github.com/docker/cagent/pkg/tui/core"
	"github.com/docker/cagent/pkg/tui/core/layout"
	"github.com/docker/cagent/pkg/tui/styles"
)

// costDialog displays detailed cost breakdown for a session.
type costDialog struct {
	BaseDialog
	keyMap  costDialogKeyMap
	session *session.Session
	offset  int
}

type costDialogKeyMap struct {
	Close, Copy, Up, Down, PageUp, PageDown key.Binding
}

var defaultCostKeyMap = costDialogKeyMap{
	Close:    key.NewBinding(key.WithKeys("esc", "enter", "q"), key.WithHelp("Esc", "close")),
	Copy:     key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "copy")),
	Up:       key.NewBinding(key.WithKeys("up", "k")),
	Down:     key.NewBinding(key.WithKeys("down", "j")),
	PageUp:   key.NewBinding(key.WithKeys("pgup")),
	PageDown: key.NewBinding(key.WithKeys("pgdown")),
}

// NewCostDialog creates a new cost dialog from session data.
func NewCostDialog(sess *session.Session) Dialog {
	return &costDialog{keyMap: defaultCostKeyMap, session: sess}
}

func (d *costDialog) Init() tea.Cmd { return nil }

func (d *costDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		cmd := d.SetSize(msg.Width, msg.Height)
		return d, cmd

	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, d.keyMap.Close):
			return d, core.CmdHandler(CloseDialogMsg{})
		case key.Matches(msg, d.keyMap.Copy):
			_ = clipboard.WriteAll(d.renderPlainText())
			return d, notification.SuccessCmd("Cost details copied to clipboard.")
		case key.Matches(msg, d.keyMap.Up):
			d.offset = max(0, d.offset-1)
		case key.Matches(msg, d.keyMap.Down):
			d.offset++
		case key.Matches(msg, d.keyMap.PageUp):
			d.offset = max(0, d.offset-d.pageSize())
		case key.Matches(msg, d.keyMap.PageDown):
			d.offset += d.pageSize()
		}

	case tea.MouseWheelMsg:
		switch msg.Button.String() {
		case "wheelup":
			d.offset = max(0, d.offset-1)
		case "wheeldown":
			d.offset++
		}
	}
	return d, nil
}

func (d *costDialog) dialogSize() (dialogWidth, maxHeight, contentWidth int) {
	dialogWidth = d.ComputeDialogWidth(70, 50, 80)
	maxHeight = min(d.Height()*70/100, 40)
	contentWidth = d.ContentWidth(dialogWidth, 2)
	return dialogWidth, maxHeight, contentWidth
}

func (d *costDialog) pageSize() int {
	_, maxHeight, _ := d.dialogSize()
	return max(1, maxHeight-10)
}

func (d *costDialog) Position() (row, col int) {
	dialogWidth, maxHeight, _ := d.dialogSize()
	return CenterPosition(d.Width(), d.Height(), dialogWidth, maxHeight)
}

func (d *costDialog) View() string {
	dialogWidth, maxHeight, contentWidth := d.dialogSize()
	content := d.renderContent(contentWidth, maxHeight)
	return styles.DialogStyle.Padding(1, 2).Width(dialogWidth).Render(content)
}

// usageInfo holds token usage and cost for a model or message.
type usageInfo struct {
	label            string
	cost             float64
	inputTokens      int64
	outputTokens     int64
	cachedTokens     int64
	cacheWriteTokens int64
}

func (u *usageInfo) totalInput() int64 {
	return u.inputTokens + u.cachedTokens + u.cacheWriteTokens
}

// cacheHitRate returns the percentage of input tokens served from cache (0-100)
// Cache hit rate = cached / (cached + new input) - cache writes don't count as "input"
func (u *usageInfo) cacheHitRate() float64 {
	totalReadTokens := u.inputTokens + u.cachedTokens
	if totalReadTokens == 0 {
		return 0
	}
	return float64(u.cachedTokens) / float64(totalReadTokens) * 100
}

// hasCacheData returns true if there's any cache activity to report
func (u *usageInfo) hasCacheData() bool {
	return u.cachedTokens > 0 || u.cacheWriteTokens > 0
}

// costData holds aggregated cost data for display.
type costData struct {
	total             usageInfo
	models            []usageInfo
	messages          []usageInfo
	tasks             []usageInfo // per-task usage for kruntime mode
	hasPerMessageData bool
}

func (d *costDialog) gatherCostData() costData {
	var data costData
	modelMap := make(map[string]*usageInfo)

	for _, msg := range d.session.GetAllMessages() {
		if msg.Message.Role != chat.MessageRoleAssistant || msg.Message.Usage == nil {
			continue
		}
		data.hasPerMessageData = true

		usage := msg.Message.Usage
		model := msg.Message.Model
		if model == "" {
			model = "unknown"
		}

		// Update totals
		data.total.cost += msg.Message.Cost
		data.total.inputTokens += usage.InputTokens
		data.total.outputTokens += usage.OutputTokens
		data.total.cachedTokens += usage.CachedInputTokens
		data.total.cacheWriteTokens += usage.CacheWriteTokens

		// Update per-model
		if modelMap[model] == nil {
			modelMap[model] = &usageInfo{label: model}
		}
		m := modelMap[model]
		m.cost += msg.Message.Cost
		m.inputTokens += usage.InputTokens
		m.outputTokens += usage.OutputTokens
		m.cachedTokens += usage.CachedInputTokens
		m.cacheWriteTokens += usage.CacheWriteTokens

		// Track per-message
		msgLabel := fmt.Sprintf("#%d", len(data.messages)+1)
		if msg.AgentName != "" {
			msgLabel = fmt.Sprintf("#%d [%s]", len(data.messages)+1, msg.AgentName)
		}
		data.messages = append(data.messages, usageInfo{
			label:            msgLabel,
			cost:             msg.Message.Cost,
			inputTokens:      usage.InputTokens,
			outputTokens:     usage.OutputTokens,
			cachedTokens:     usage.CachedInputTokens,
			cacheWriteTokens: usage.CacheWriteTokens,
		})
	}

	// Convert model map to sorted slice (by cost descending)
	for _, m := range modelMap {
		data.models = append(data.models, *m)
	}
	sort.Slice(data.models, func(i, j int) bool {
		return data.models[i].cost > data.models[j].cost
	})

	// Fall back to session-level totals if no per-message data
	if !data.hasPerMessageData {
		data.total = usageInfo{
			cost:         d.session.Cost,
			inputTokens:  d.session.InputTokens,
			outputTokens: d.session.OutputTokens,
		}
	}

	// Gather per-task usage data (kruntime mode)
	// In kruntime mode, task-based totals are authoritative because some messages
	// (especially task_complete responses) may not have usage data attached.
	var taskTotals usageInfo
	for i, task := range d.session.Tasks {
		// Only include tasks that have usage data
		if task.InputTokens == 0 && task.OutputTokens == 0 && task.Cost == 0 {
			continue
		}

		// Accumulate task totals
		taskTotals.cost += task.Cost
		taskTotals.inputTokens += task.InputTokens
		taskTotals.outputTokens += task.OutputTokens
		taskTotals.cachedTokens += task.CachedInputTokens
		taskTotals.cacheWriteTokens += task.CacheWriteTokens

		label := fmt.Sprintf("Task #%d", i+1)

		data.tasks = append(data.tasks, usageInfo{
			label:            label,
			cost:             task.Cost,
			inputTokens:      task.InputTokens,
			outputTokens:     task.OutputTokens,
			cachedTokens:     task.CachedInputTokens,
			cacheWriteTokens: task.CacheWriteTokens,
		})
	}

	// In kruntime mode, use task-based totals as they are more accurate.
	// Message-based totals may be incomplete because some responses (like task_complete)
	// don't have usage data attached to the session message.
	if len(data.tasks) > 0 {
		data.total = taskTotals
	}

	return data
}

func (d *costDialog) renderContent(contentWidth, maxHeight int) string {
	data := d.gatherCostData()

	// Build all lines with improved visual hierarchy
	lines := []string{
		RenderTitle("Session Cost Details", contentWidth, styles.DialogTitleStyle),
		RenderSeparator(contentWidth),
		"",
	}

	// Total section with clear visual prominence
	lines = append(lines,
		d.renderSectionHeader("Total"),
		"",
		fmt.Sprintf("  %s  %s",
			costBadgeStyle.Render(fmt.Sprintf(" %s ", formatCost(data.total.cost))),
			styles.MutedStyle.Render("estimated session cost")),
		"",
	)

	// Token breakdown as clean table
	lines = append(lines,
		fmt.Sprintf("  %s  %s",
			columnLabelStyle.Render("Input "),
			columnValueStyle.Render(padTokens(formatTokenCount(data.total.totalInput())))),
		fmt.Sprintf("  %s  %s",
			columnLabelStyle.Render("Output"),
			columnValueStyle.Render(padTokens(formatTokenCount(data.total.outputTokens)))),
	)

	// Show cache stats if there's cache activity
	if data.total.hasCacheData() {
		hitRate := data.total.cacheHitRate()
		hitRateStyle := dimValueStyle
		if hitRate >= 50 {
			hitRateStyle = cacheHitHighStyle
		} else if hitRate >= 20 {
			hitRateStyle = cacheHitMedStyle
		}
		lines = append(lines,
			"",
			fmt.Sprintf("  %s  %s  %s",
				columnLabelStyle.Render("Cache "),
				hitRateStyle.Render(fmt.Sprintf("%5.1f%% hit", hitRate)),
				dimValueStyle.Render(fmt.Sprintf("(%s cached, %s new, %s written)",
					formatTokenCount(data.total.cachedTokens),
					formatTokenCount(data.total.inputTokens),
					formatTokenCount(data.total.cacheWriteTokens)))),
		)
	}
	lines = append(lines, "")

	// By Model Section
	if len(data.models) > 0 {
		lines = append(lines, d.renderSectionHeader("By Model"), "")
		for _, m := range data.models {
			lines = append(lines, d.renderCompactUsageLine(m))
		}
		lines = append(lines, "")
	}

	// By Task Section (kruntime mode)
	if len(data.tasks) > 0 {
		lines = append(lines, d.renderSectionHeader("By Task"), "")
		for _, t := range data.tasks {
			lines = append(lines, d.renderTaskLines(t)...)
		}
		lines = append(lines, "")
	}

	// By Message Section
	if len(data.messages) > 0 {
		lines = append(lines, d.renderSectionHeader("By Message"), "")
		for _, m := range data.messages {
			lines = append(lines, d.renderCompactUsageLine(m))
		}
		lines = append(lines, "")
	} else if !data.hasPerMessageData && data.total.cost > 0 {
		lines = append(lines, styles.MutedStyle.Render("  Per-message breakdown not available."), "")
	}

	// Apply scrolling
	return d.applyScrolling(lines, contentWidth, maxHeight)
}

func (d *costDialog) renderSectionHeader(title string) string {
	return fmt.Sprintf("%s %s", sectionBulletStyle.Render("▸"), sectionStyle.Render(title))
}

// renderCompactUsageLine renders a single-line usage summary with aligned columns
func (d *costDialog) renderCompactUsageLine(u usageInfo) string {
	return fmt.Sprintf("  %s  %s %s  %s %s  %s",
		costStyle.Render(padCost(formatCost(u.cost))),
		dimLabelStyle.Render("in"),
		columnValueStyle.Render(padTokens(formatTokenCount(u.totalInput()))),
		dimLabelStyle.Render("out"),
		columnValueStyle.Render(padTokens(formatTokenCount(u.outputTokens))),
		labelStyle.Render(u.label))
}

// renderTaskLines renders a task with cost, context size, output, and cache hit rate
func (d *costDialog) renderTaskLines(u usageInfo) []string {
	// Show: cost | context size | output tokens | cache hit% | task label
	cacheStr := "  -  "
	if u.hasCacheData() {
		hitRate := u.cacheHitRate()
		cacheStr = fmt.Sprintf("%4.0f%%", hitRate)
		// Color based on hit rate
		if hitRate >= 50 {
			cacheStr = cacheHitHighStyle.Render(cacheStr)
		} else if hitRate >= 20 {
			cacheStr = cacheHitMedStyle.Render(cacheStr)
		} else {
			cacheStr = cacheHitLowStyle.Render(cacheStr)
		}
	} else {
		cacheStr = dimValueStyle.Render(cacheStr)
	}

	return []string{
		fmt.Sprintf("  %s  %s %s  %s %s  %s  %s",
			costStyle.Render(padCost(formatCost(u.cost))),
			dimLabelStyle.Render("ctx"),
			columnValueStyle.Render(padTokens(formatTokenCount(u.totalInput()))),
			dimLabelStyle.Render("out"),
			columnValueStyle.Render(padTokens(formatTokenCount(u.outputTokens))),
			cacheStr,
			taskLabelStyle.Render(u.label)),
	}
}

func (d *costDialog) applyScrolling(allLines []string, contentWidth, maxHeight int) string {
	const headerLines = 3 // title + separator + space
	const footerLines = 2 // space + help

	visibleLines := max(1, maxHeight-headerLines-footerLines-4)
	contentLines := allLines[headerLines:]
	totalContentLines := len(contentLines)

	// Clamp offset
	maxOffset := max(0, totalContentLines-visibleLines)
	d.offset = min(d.offset, maxOffset)

	// Extract visible portion
	endIdx := min(d.offset+visibleLines, totalContentLines)
	parts := append(allLines[:headerLines], contentLines[d.offset:endIdx]...)

	// Scroll indicator
	if totalContentLines > visibleLines {
		scrollInfo := fmt.Sprintf("[%d-%d of %d]", d.offset+1, endIdx, totalContentLines)
		if d.offset > 0 {
			scrollInfo = "↑ " + scrollInfo
		}
		if endIdx < totalContentLines {
			scrollInfo += " ↓"
		}
		parts = append(parts, styles.MutedStyle.Render(scrollInfo))
	}

	parts = append(parts, "", RenderHelpKeys(contentWidth, "↑↓", "scroll", "c", "copy", "Esc", "close"))
	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

func (d *costDialog) renderPlainText() string {
	data := d.gatherCostData()
	var lines []string

	// Build input line with optional breakdown
	inputLine := fmt.Sprintf("input: %s", formatTokenCount(data.total.totalInput()))
	if data.total.cachedTokens > 0 || data.total.cacheWriteTokens > 0 {
		inputLine += fmt.Sprintf(" (%s new + %s cached + %s cache write)",
			formatTokenCount(data.total.inputTokens),
			formatTokenCount(data.total.cachedTokens),
			formatTokenCount(data.total.cacheWriteTokens))
	}

	lines = append(lines, "Session Cost Details", "", "Total", formatCost(data.total.cost),
		inputLine, fmt.Sprintf("output: %s", formatTokenCount(data.total.outputTokens)), "")

	if len(data.models) > 0 {
		lines = append(lines, "By Model")
		for _, m := range data.models {
			lines = append(lines, formatPlainTextUsageLine(m))
		}
		lines = append(lines, "")
	}

	if len(data.tasks) > 0 {
		lines = append(lines, "By Task")
		for _, t := range data.tasks {
			lines = append(lines, formatPlainTextUsageLineExpanded(t)...)
		}
		lines = append(lines, "")
	}

	if len(data.messages) > 0 {
		lines = append(lines, "By Message")
		for _, m := range data.messages {
			lines = append(lines, formatPlainTextUsageLine(m))
		}
	}

	return strings.Join(lines, "\n")
}

// Styles for the cost dialog with improved visual hierarchy
var (
	// Section headers
	sectionStyle       = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(styles.ColorWhite))
	sectionBulletStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(styles.ColorHighlight))

	// Cost badge for total
	costBadgeStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color(styles.ColorBackground)).
			Background(lipgloss.Color(styles.ColorHighlight)).
			Bold(true)

	// Regular cost display
	costStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(styles.ColorHighlight))

	// Column labels and values
	columnLabelStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(styles.ColorTextSecondary))
	columnValueStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(styles.ColorWhite))

	// Dim labels and values for secondary info
	dimLabelStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(styles.ColorTextSecondary))
	dimValueStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(styles.ColorMutedGray))

	// Task and item labels
	labelStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color(styles.ColorTextPrimary))
	taskLabelStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(styles.ColorAccentBlue))

	// Cache hit rate styles (color coded: green=good, yellow=ok, red=poor)
	cacheHitHighStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(styles.ColorSuccessGreen))
	cacheHitMedStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color(styles.ColorWarningYellow))
	cacheHitLowStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color(styles.ColorErrorRed))

	// Legacy styles for compatibility
	accentStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(styles.ColorHighlight))
)

func formatCost(cost float64) string {
	if cost < 0.0001 {
		return "$0.00"
	}
	if cost < 0.01 {
		return fmt.Sprintf("$%.4f", cost)
	}
	return fmt.Sprintf("$%.2f", cost)
}

func formatTokenCount(count int64) string {
	switch {
	case count >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(count)/1_000_000)
	case count >= 1_000:
		return fmt.Sprintf("%.1fK", float64(count)/1_000)
	default:
		return fmt.Sprintf("%d", count)
	}
}

// padCost pads cost strings to align columns (7 chars for "$X.XXXX")
func padCost(s string) string {
	const width = 7
	if len(s) >= width {
		return s
	}
	return s + strings.Repeat(" ", width-len(s))
}

// padTokens pads token count strings to align columns
func padTokens(s string) string {
	const width = 6
	if len(s) >= width {
		return s
	}
	return strings.Repeat(" ", width-len(s)) + s
}

// formatPlainTextUsageLine formats a usage line for plain text output
func formatPlainTextUsageLine(u usageInfo) string {
	return fmt.Sprintf("%-8s  in: %-8s  out: %-8s  %s",
		padCost(formatCost(u.cost)), formatTokenCount(u.totalInput()), formatTokenCount(u.outputTokens), u.label)
}

// formatPlainTextUsageLineExpanded formats a task line for plain text output
func formatPlainTextUsageLineExpanded(u usageInfo) []string {
	return []string{
		fmt.Sprintf("%-8s  ctx: %-8s  out: %-8s  %s",
			padCost(formatCost(u.cost)), formatTokenCount(u.totalInput()), formatTokenCount(u.outputTokens), u.label),
	}
}
