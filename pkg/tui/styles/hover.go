package styles

import "charm.land/lipgloss/v2"

const HoverBrightenAmount = 0.35

func Hovered(base lipgloss.Style) lipgloss.Style {
	s := base.Bold(true)
	fg := base.GetForeground()
	if fg == nil {
		return s
	}
	if _, isNoColor := fg.(lipgloss.NoColor); isNoColor {
		return s
	}
	return s.Foreground(BrightenColor(fg, HoverBrightenAmount))
}
