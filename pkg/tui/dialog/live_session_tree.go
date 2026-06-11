package dialog

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

type liveSessionTreeDialog struct {
	tree     *runtime.LiveSessionTree
	items    []*runtime.LiveSessionNode
	selected int
	width    int
	height   int
}

func NewLiveSessionTreeDialog(tree *runtime.LiveSessionTree) Dialog {
	d := &liveSessionTreeDialog{tree: tree}
	d.rebuild()
	return d
}

func (d *liveSessionTreeDialog) Init() tea.Cmd { return nil }

func (d *liveSessionTreeDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		d.width = msg.Width
		d.height = msg.Height
	case tea.KeyPressMsg:
		switch msg.String() {
		case "esc", "ctrl+c":
			return d, core.CmdHandler(CloseDialogMsg{})
		case "up", "k":
			if d.selected > 0 {
				d.selected--
			}
		case "down", "j":
			if d.selected < len(d.items)-1 {
				d.selected++
			}
		case "enter":
			if d.selected >= 0 && d.selected < len(d.items) {
				return d, core.CmdHandler(messages.AttachSessionMsg{SessionID: d.items[d.selected].ID})
			}
		}
	}
	return d, nil
}

func (d *liveSessionTreeDialog) View() string {
	var lines []string
	if d.tree == nil || d.tree.Root == nil || len(d.items) == 0 {
		lines = []string{styles.MutedStyle.Render("No live subagents")}
	} else {
		for i, node := range d.items {
			prefix := strings.Repeat("  ", max(0, node.Depth-1))
			status := "○"
			if node.Live {
				status = "●"
			}
			label := node.AgentName
			if label == "" {
				label = shortNodeID(node.ID)
			}
			line := fmt.Sprintf("%s%s %s", prefix, status, label)
			if node.Preview != "" {
				line += " — " + node.Preview
			}
			if i == d.selected {
				line = styles.CompletionSelectedStyle.Render(line)
			}
			lines = append(lines, line)
		}
	}
	body := strings.Join(lines, "\n")
	return styles.DialogStyle.Width(72).Render(
		styles.DialogTitleStyle.Render("Live subagents") + "\n\n" +
			body + "\n\n" +
			styles.MutedStyle.Render("↑/↓ select • enter attach • esc close"),
	)
}

func (d *liveSessionTreeDialog) SetSize(width, height int) tea.Cmd {
	d.width = width
	d.height = height
	return nil
}

func (d *liveSessionTreeDialog) Position() (int, int) {
	view := d.View()
	return CenterPosition(d.width, d.height, lipgloss.Width(view), lipgloss.Height(view))
}

func (d *liveSessionTreeDialog) rebuild() {
	d.items = nil
	if d.tree == nil || d.tree.Root == nil {
		return
	}
	var walk func(*runtime.LiveSessionNode)
	walk = func(node *runtime.LiveSessionNode) {
		if node == nil {
			return
		}
		if node.Depth > 0 {
			d.items = append(d.items, node)
		}
		for _, child := range node.Children {
			walk(child)
		}
	}
	walk(d.tree.Root)
	if d.selected >= len(d.items) {
		d.selected = max(0, len(d.items)-1)
	}
}

func shortNodeID(id string) string {
	if len(id) <= 5 {
		return id
	}
	return id[:5]
}
