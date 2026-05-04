package styles

import (
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHovered_BoldensAndBrightensForeground(t *testing.T) {
	t.Parallel()

	base := lipgloss.NewStyle().Foreground(lipgloss.Color("#336699"))
	hovered := Hovered(base)

	assert.True(t, hovered.GetBold(), "hovered style should be bold")

	baseFg := base.GetForeground()
	hoverFg := hovered.GetForeground()
	require.NotNil(t, baseFg)
	require.NotNil(t, hoverFg)

	assert.Greater(t,
		relativeLuminanceColor(hoverFg),
		relativeLuminanceColor(baseFg),
		"hovered foreground should be brighter than the base foreground")
}

func TestHovered_NoColorOnlyAddsBold(t *testing.T) {
	t.Parallel()

	base := lipgloss.NewStyle()
	hovered := Hovered(base)

	assert.True(t, hovered.GetBold())
	_, isNoColor := hovered.GetForeground().(lipgloss.NoColor)
	assert.True(t, isNoColor,
		"styles without an explicit foreground should not invent one on hover")
}

func TestHovered_Repeatable(t *testing.T) {
	t.Parallel()

	base := lipgloss.NewStyle().Foreground(lipgloss.Color("#7a5af8"))
	a := Hovered(base)
	b := Hovered(base)

	assert.Equal(t, a.GetBold(), b.GetBold())
	assert.Equal(t, a.Render("x"), b.Render("x"),
		"hovered style generation must be deterministic for stable rendering/tests")
}
