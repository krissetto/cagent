// Package subagenttree renders a live, self-updating tree of the running
// subagent swarm for the TUI. It is deliberately minimal: it subscribes to a
// [subagent.Tree], turns each published snapshot into a bubbletea message, and
// renders an indented tree with a per-node state glyph. All async coupling to
// bubbletea is confined to a single listen command.
package subagenttree

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/subagent"
)

// snapshotMsg carries a new tree snapshot into the bubbletea update loop.
type snapshotMsg struct {
	snapshot subagent.Snapshot
	ch       <-chan subagent.Snapshot
}

// Model is a read-only, live-updating view of the subagent tree.
type Model struct {
	tree     *subagent.Tree
	ch       <-chan subagent.Snapshot
	cancel   func()
	snapshot subagent.Snapshot
	width    int
}

// New builds a Model bound to a tree. Call [Model.Init] to begin listening and
// [Model.Close] when the view goes away.
func New(tree *subagent.Tree) *Model {
	return &Model{tree: tree}
}

// Init subscribes to the tree and starts listening for snapshots.
func (m *Model) Init() tea.Cmd {
	if m.tree == nil {
		return nil
	}
	ch, cancel := m.tree.Subscribe(8)
	m.ch = ch
	m.cancel = cancel
	return listen(ch)
}

// Update handles snapshot and resize messages. It ignores everything else,
// keeping its footprint on the bubbletea event system minimal.
func (m *Model) Update(msg tea.Msg) (*Model, tea.Cmd) {
	switch msg := msg.(type) {
	case snapshotMsg:
		if msg.ch != m.ch {
			return m, nil // stale listener from a previous subscription
		}
		m.snapshot = msg.snapshot
		return m, listen(m.ch)
	case tea.WindowSizeMsg:
		m.width = msg.Width
	}
	return m, nil
}

// Close cancels the tree subscription.
func (m *Model) Close() {
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
}

// View renders the current tree.
func (m *Model) View() string {
	if len(m.snapshot.Nodes) == 0 {
		return "No subagents running."
	}
	return Render(m.snapshot.Nodes)
}

// Render formats a list of node snapshots as an indented tree with per-node
// state glyphs. It is exported so other views (e.g. the sidebar) can render a
// subtree without embedding the full bubbletea component.
func Render(nodes []subagent.NodeSnapshot) string {
	var b strings.Builder
	for i := range nodes {
		renderNode(&b, nodes[i], 0)
	}
	return strings.TrimRight(b.String(), "\n")
}

func renderNode(b *strings.Builder, n subagent.NodeSnapshot, depth int) {
	indent := strings.Repeat("  ", depth)
	fmt.Fprintf(b, "%s%s %s", indent, glyph(n.Node.State), n.Node.DisplayName())
	if label := stateLabel(n.Node); label != "" {
		fmt.Fprintf(b, " (%s)", label)
	}
	b.WriteByte('\n')
	for i := range n.Children {
		renderNode(b, n.Children[i], depth+1)
	}
}

// glyph maps a node state to a compact status marker.
func glyph(s subagent.NodeState) string {
	switch s {
	case subagent.NodeRunning:
		return "▶"
	case subagent.NodeIdle:
		return " "
	case subagent.NodeCompleted:
		return "✓"
	case subagent.NodeFailed:
		return "✗"
	case subagent.NodeStopped:
		return "■"
	default:
		return "…"
	}
}

// stateLabel returns a short human label for terminal/interesting states.
func stateLabel(n subagent.Node) string {
	switch n.State {
	case subagent.NodeFailed:
		if n.Error != "" {
			return "failed: " + firstLine(n.Error)
		}
		return "failed"
	case subagent.NodeIdle:
		return "idle"
	case subagent.NodeRunning:
		return "running"
	default:
		return string(n.State)
	}
}

func firstLine(s string) string {
	if before, _, found := strings.Cut(s, "\n"); found {
		return before
	}
	return s
}

// listen blocks on the subscription channel and delivers the next snapshot as a
// message. Returning it again from Update keeps a single in-flight read.
func listen(ch <-chan subagent.Snapshot) tea.Cmd {
	return func() tea.Msg {
		snap, ok := <-ch
		if !ok {
			return nil
		}
		return snapshotMsg{snapshot: snap, ch: ch}
	}
}
