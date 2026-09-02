package ui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestRenderUserLinesUsesDistinctFullWidthBackground(t *testing.T) {
	t.Parallel()

	const width = 24
	lines := RenderUserLines("make this visible", width)
	require.NotEmpty(t, lines)
	for _, line := range lines {
		assert.Equal(t, width, DisplayWidth(line))
	}

	style := StUserBox(width)
	assert.Equal(t, styles.MobyBlue, style.GetBackground())
	assert.NotEqual(t, StToolBox(width).GetBackground(), style.GetBackground())
}

func TestRenderUserLinesWrapsInsideBackgroundPadding(t *testing.T) {
	t.Parallel()

	const width = 12
	lines := RenderUserLines("a user message that wraps", width)
	require.Greater(t, len(lines), 1)
	for _, line := range lines {
		assert.LessOrEqual(t, DisplayWidth(line), width)
	}
}
