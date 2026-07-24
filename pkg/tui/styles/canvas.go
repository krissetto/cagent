package styles

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"
)

// LayerInfo contains the information needed to manually compose a layer.
// This is used as a workaround for lipgloss v2.0.0's canvas.Compose() bug.
type LayerInfo struct {
	Content string
	X, Y    int
}

// ComposeLayersManually composes multiple layers into a single string.
// This is a workaround for lipgloss v2.0.0's canvas.Compose() bug where it replaces
// content instead of overlaying.
//
// Layers are applied in order; later layers appear on top of earlier ones.
// ANSI escape codes are preserved for all text outside of each overlay region.
func ComposeLayersManually(width, height int, layers ...LayerInfo) string {
	if len(layers) == 0 {
		return ""
	}

	// Start with an empty canvas — initialize from the first (base) layer.
	lines := make([]string, height)
	for i := range lines {
		lines[i] = strings.Repeat(" ", width)
	}

	// Apply each layer onto the canvas.
	for _, layer := range layers {
		contentLines := strings.Split(layer.Content, "\n")

		for lineIdx, lineContent := range contentLines {
			targetY := layer.Y + lineIdx
			if targetY < 0 || targetY >= height {
				continue
			}

			lines[targetY] = overlayLine(lines[targetY], lineContent, layer.X, width)
		}
	}

	return strings.Join(lines, "\n")
}

// overlayLine overlays content onto a base line at the given x position.
//
// It uses ansi.Truncate / ansi.TruncateLeft to split the base line at exact
// visual column boundaries so that all ANSI styling (colors, dim, bold, etc.)
// on the base is preserved verbatim outside the overlay region.
func overlayLine(base, overlay string, xPos, totalWidth int) string {
	if overlay == "" {
		return base
	}
	if xPos < 0 || xPos >= totalWidth {
		return base
	}

	// Measure how many visual columns the overlay occupies, and clip it to the
	// available canvas width so over-wide animated layers cannot bleed past the
	// right edge.
	overlayWidth := runewidth.StringWidth(ansi.Strip(overlay))
	if overlayWidth == 0 {
		return base
	}
	if xPos+overlayWidth > totalWidth {
		overlay = ansi.Cut(overlay, 0, totalWidth-xPos)
		overlayWidth = runewidth.StringWidth(ansi.Strip(overlay))
		if overlayWidth == 0 {
			return base
		}
	}

	// Left portion: columns [0, xPos) — preserve base styling.
	leftPart := ansi.Truncate(base, xPos, "")

	// Right portion: columns [xPos+overlayWidth, ∞) — preserve base styling.
	rightPart := ansi.TruncateLeft(base, xPos+overlayWidth, "")

	return leftPart + overlay + rightPart
}
