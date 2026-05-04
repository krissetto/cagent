package styles

import (
	"charm.land/lipgloss/v2"
)

// HoverBrightenAmount is the fraction (0..1) by which the Hovered helper
// lifts a style's foreground color toward white. It is centralised here so
// every hover-highlighted element in the TUI brightens by the same amount
// regardless of the active theme.
//
// 0.35 was chosen empirically: it visibly pops against the normal foreground
// on both dark and light themes without turning saturated colors into pure
// white. Adjust here to retune the whole TUI's hover intensity at once.
const HoverBrightenAmount = 0.35

// Hovered returns a hover-emphasized variant of base.
//
// The foreground color is brightened by [HoverBrightenAmount] via
// [BrightenColor] and bold is applied on top. This gives hovered elements a
// reliable, theme-agnostic visual lift: hue is preserved, saturation is
// nudged for dark colors, and lightness is pulled toward white by a fixed
// fraction — all without hard-coding per-theme overrides.
//
// If base has no explicit foreground color (lipgloss.NoColor) Hovered only
// applies bold. We deliberately do not invent a color out of thin air: without
// a theme-provided starting point we have nothing trustworthy to brighten, and
// forcing a color would fight whatever terminal default the user already sees.
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
