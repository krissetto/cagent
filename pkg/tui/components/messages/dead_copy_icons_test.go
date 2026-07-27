package messages

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/markdown"
	"github.com/docker/docker-agent/pkg/tui/service"
)

// Every rendered code-block copy icon must be clickable: views that cannot
// hit-test the click (reasoning blocks) must not render the icon at all,
// while message views (shell output, welcome) must map the click to a copy
// command. This guards against dead copy buttons that silently do nothing.
func TestCodeBlockCopyIconsAreNeverDead(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		setup    func(m *model)
		wantIcon bool
	}{
		"reasoning block renders no copy icon": {
			setup: func(m *model) {
				m.AppendReasoning("root", "Let me think.\n\n```go\nx := 1\n```\n\nDone thinking.")
			},
			wantIcon: false,
		},
		"shell output copy icon is clickable": {
			setup: func(m *model) {
				m.AddShellOutputMessage("total 0\ndrwxr-xr-x 2 x x 64 Jan 1 .")
			},
			wantIcon: true,
		},
		"welcome message copy icon is clickable": {
			setup: func(m *model) {
				m.AddWelcomeMessage("Hello\n\n```bash\nrun me\n```\n")
			},
			wantIcon: true,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			m := NewScrollableView(animation.NewRuntime(), 80, 40, &service.SessionState{}).(*model)
			m.SetSize(80, 40)
			tc.setup(m)
			m.renderDirty = true
			m.View()
			m.ensureAllItemsRendered()

			found := false
			for line, rendered := range m.renderedLines {
				plain := ansi.Strip(rendered)
				before, _, ok := strings.Cut(plain, markdown.CodeBlockCopyIcon)
				if !ok {
					continue
				}
				found = true
				start := ansi.StringWidth(before)
				width := ansi.StringWidth(markdown.CodeBlockCopyIcon)
				for col := start; col < start+width; col++ {
					_, cmd := m.handleMouseClick(tea.MouseClickMsg{X: col, Y: line, Button: tea.MouseLeft})
					assert.NotNil(t, cmd, "copy icon click at line=%d col=%d must copy (plain=%q)", line, col, plain)
				}
			}
			require.Equal(t, tc.wantIcon, found, "copy icon presence mismatch")
		})
	}
}
