package tabbar

import (
	"image/color"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

const (
	defaultMaxTitleLen = 20
	defaultTabTitle    = "New Session"
	closeButtonText    = " ×"
	accentBar          = "▎"
	tabChrome          = 4
	indicatorSlotWidth = 2

	dragSourceColorBoost   = 0.4
	dragBystanderDimAmount = 0.65
)

// FixedTabWidth returns the stable rendered width of a tab. Animated indicators
// borrow from the title budget rather than changing tab geometry.
func FixedTabWidth(maxTitleLen int) int { return maxTitleLen + tabChrome }

// Tab represents a single rendered tab in the tab bar.
type Tab struct {
	view        string
	mainZoneEnd int
	width       int
}

func (t Tab) View() string     { return t.view }
func (t Tab) Width() int       { return t.width }
func (t Tab) MainZoneEnd() int { return t.mainZoneEnd }

type dragRole int

const (
	dragRoleNone dragRole = iota
	dragRoleSource
	dragRoleBystander
)

func blendColors(a, b color.Color, ratio float64) color.Color {
	ar, ag, ab := styles.ColorToRGB(a)
	br, bg, bb := styles.ColorToRGB(b)
	return styles.RGBToColor(ar+(br-ar)*ratio, ag+(bg-ag)*ratio, ab+(bb-ab)*ratio)
}

// renderTab matches the reference fixed geometry while retaining Docker
// Agent's product-specific tab content (title, running and attention states).
func renderTab(info messages.TabInfo, maxTitleLen int, role dragRole, elapsed time.Duration) Tab {
	hasIndicator := info.NeedsAttention || info.IsRunning
	titleBudget := maxTitleLen
	if hasIndicator {
		titleBudget -= indicatorSlotWidth
	}
	titleBudget = max(0, titleBudget)

	title := info.Title
	if title == "" {
		title = defaultTabTitle
	}
	if lipgloss.Width(title) > titleBudget {
		if titleBudget > 0 {
			title = ansi.Truncate(title, titleBudget, "…")
		} else {
			title = ""
		}
	}

	var bgColor, fgColor, barColor color.Color
	if info.IsActive {
		bgColor = styles.TabBg
		fgColor = styles.TabActiveFg
		barColor = styles.Accent
	} else {
		bgColor = styles.Background
		fgColor = styles.TabInactiveFg
		barColor = styles.TabInactiveFg
	}
	if role == dragRoleSource && !info.IsActive {
		fgColor = blendColors(fgColor, styles.TabActiveFg, dragSourceColorBoost)
		barColor = blendColors(barColor, styles.Accent, dragSourceColorBoost)
	}
	closeFg := styles.MutedContrastFg(bgColor)
	if role == dragRoleBystander {
		fgColor = blendColors(fgColor, bgColor, dragBystanderDimAmount)
		barColor = blendColors(barColor, bgColor, dragBystanderDimAmount)
		closeFg = blendColors(closeFg, bgColor, dragBystanderDimAmount)
	}

	pad := lipgloss.NewStyle().Background(bgColor)
	bar := lipgloss.NewStyle().Foreground(barColor).Background(bgColor).Render(accentBar)

	indicator := ""
	if hasIndicator {
		switch {
		case info.IsRunning:
			fg := styles.EnsureContrast(styles.TabAccentFg, bgColor)
			if role == dragRoleBystander {
				fg = blendColors(fg, bgColor, dragBystanderDimAmount)
			}
			indicator = lipgloss.NewStyle().Foreground(fg).Background(bgColor).Render(animation.TabBusy.FrameAt(elapsed) + " ")
		case info.NeedsAttention:
			fg := styles.EnsureContrast(styles.Warning, bgColor)
			if role == dragRoleBystander {
				fg = blendColors(fg, bgColor, dragBystanderDimAmount)
			}
			indicator = lipgloss.NewStyle().Foreground(fg).Background(bgColor).Bold(true).Render("! ")
		}
	}

	titleStyle := lipgloss.NewStyle().Foreground(fgColor).Background(bgColor)
	if info.IsActive {
		titleStyle = titleStyle.Bold(true)
	}
	trailingPad := max(0, titleBudget-lipgloss.Width(title))
	content := bar + indicator + titleStyle.Render(title) + pad.Render(spacer(trailingPad))
	mainEnd := lipgloss.Width(content)
	content += lipgloss.NewStyle().Foreground(closeFg).Background(bgColor).Render(closeButtonText) + pad.Render(" ")

	return Tab{view: content, mainZoneEnd: mainEnd, width: lipgloss.Width(content)}
}
