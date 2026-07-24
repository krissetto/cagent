package editor

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

const (
	bannerContentOffset        = 2 // left padding applied to the bar content
	contextBarChevronExpanded  = "▾"
	contextBarChevronCollapsed = "▸"
	contextBarMarginTop        = 1
)

// contextBar renders the expandable bar above the editor that displays
// attachment pills.
type contextBar struct {
	attachments    []bannerItem
	height         int
	lastInnerWidth int
	regions        []bannerRegion
	expanded       bool
	focused        bool
}

type bannerItem struct {
	label       string
	placeholder string
}

type bannerRegion struct {
	start int
	end   int
	item  bannerItem
}

func newContextBar() *contextBar {
	return &contextBar{}
}

func (b *contextBar) SetItems(items []bannerItem) {
	b.attachments = items
	b.updateHeight()
}

func (b *contextBar) Height() int {
	return b.height
}

func (b *contextBar) IsExpanded() bool {
	return b.expanded
}

func (b *contextBar) Toggle() {
	b.expanded = !b.expanded
	b.updateHeight()
}

func (b *contextBar) SetFocused(focused bool) {
	b.focused = focused
}

func (b *contextBar) hasContent() bool {
	return len(b.attachments) > 0
}

func (b *contextBar) updateHeight() {
	if !b.hasContent() {
		b.height = 0
		return
	}
	// top border + summary row
	b.height = 2 + contextBarMarginTop
}

func (b *contextBar) View(totalWidth int) string {
	if !b.hasContent() {
		return ""
	}

	innerWidth := totalWidth - 2*styles.AppPadding
	b.lastInnerWidth = innerWidth
	b.updateHeight()

	var rows []string
	rows = append(rows, b.renderTopBorder(innerWidth), b.renderSummaryRow(innerWidth))

	content := strings.Join(rows, "\n")
	padStyle := lipgloss.NewStyle().Padding(0, styles.AppPadding).MarginTop(contextBarMarginTop)
	if b.focused {
		padStyle = padStyle.Background(styles.Selected)
	}
	return padStyle.Render(content)
}

func (b *contextBar) renderTopBorder(innerWidth int) string {
	if innerWidth <= 0 {
		return ""
	}
	return styles.ResizeHandleStyle.Render(strings.Repeat("─", innerWidth))
}

// renderSummaryRow renders the single collapsed row with attachment pills on the left
// and summary labels (attachment count) on the right.
func (b *contextBar) renderSummaryRow(innerWidth int) string {
	// Build left side: attachment pills
	var leftParts []string
	if len(b.attachments) > 0 {
		var pills []string
		for _, item := range b.attachments {
			name, size := parseLabel(item.label)
			pill := styles.AttachmentIconStyle.Render("📎 ") +
				styles.AttachmentBadgeStyle.Render(name)
			if size != "" {
				pill += " " + styles.AttachmentSizeStyle.Render(size)
			}
			pills = append(pills, pill)
		}
		separator := "  "
		leftParts = append(leftParts, strings.Join(pills, separator))
		b.buildRegions(pills, separator)
	} else {
		b.regions = b.regions[:0]
	}

	left := strings.Join(leftParts, "  ")

	// Build right side: summary labels
	var rightParts []string
	if len(b.attachments) > 0 {
		countLabel := fmt.Sprintf("%d attachment", len(b.attachments))
		if len(b.attachments) != 1 {
			countLabel += "s"
		}
		rightParts = append(rightParts, styles.AttachmentSizeStyle.Render(countLabel))
	}

	chevron := contextBarChevronCollapsed
	if b.expanded {
		chevron = contextBarChevronExpanded
	}
	right := strings.Join(rightParts, "  ") + " " + styles.MutedStyle.Render(chevron)

	return b.alignRow(left, right, innerWidth)
}

// alignRow left-aligns the main content and right-aligns the summary label within the given width.
func (b *contextBar) alignRow(left, right string, innerWidth int) string {
	leftWidth := ansi.StringWidth(left)
	rightWidth := ansi.StringWidth(right)
	gap := innerWidth - leftWidth - rightWidth
	gap = max(gap, 2)
	return left + strings.Repeat(" ", gap) + right
}

func (b *contextBar) buildRegions(pills []string, separator string) {
	b.regions = b.regions[:0]
	if len(pills) == 0 {
		return
	}

	pos := 0
	sepWidth := ansi.StringWidth(separator)

	for i, pill := range pills {
		if i > 0 {
			pos += sepWidth
		}
		width := ansi.StringWidth(pill)
		b.regions = append(b.regions, bannerRegion{
			start: pos,
			end:   pos + width,
			item:  b.attachments[i],
		})
		pos += width
	}
}

func (b *contextBar) HitTest(x int) (bannerItem, bool) {
	if len(b.regions) == 0 {
		return bannerItem{}, false
	}

	rel := x - bannerContentOffset
	if rel < 0 {
		return bannerItem{}, false
	}

	for _, region := range b.regions {
		if rel >= region.start && rel < region.end {
			return region.item, true
		}
	}
	return bannerItem{}, false
}

// parseLabel splits a label like "paste-1 (21.1 KB)" into name and size parts.
func parseLabel(label string) (name, size string) {
	idx := strings.LastIndex(label, " (")
	if idx > 0 && strings.HasSuffix(label, ")") {
		return label[:idx], label[idx+1:]
	}
	return label, ""
}

// softWrap breaks a string into lines that fit within maxWidth.
//
//nolint:unused // Retained for context-bar wrapping compatibility tests.
func softWrap(s string, maxWidth int) []string {
	if maxWidth <= 0 {
		return []string{s}
	}
	width := ansi.StringWidth(s)
	if width <= maxWidth {
		return []string{s}
	}

	var lines []string
	runes := []rune(s)
	for len(runes) > 0 {
		// Find how many runes fit within maxWidth
		end := len(runes)
		for end > 0 && ansi.StringWidth(string(runes[:end])) > maxWidth {
			end--
		}
		if end == 0 {
			end = 1 // always take at least one rune to avoid infinite loop
		}
		lines = append(lines, string(runes[:end]))
		runes = runes[end:]
	}
	return lines
}
