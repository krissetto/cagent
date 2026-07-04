package subagenttree

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/subagent"
)

func TestViewEmpty(t *testing.T) {
	m := New(subagent.NewTree())
	assert.Equal(t, "No subagents running.", m.View())
}

func TestViewRendersTree(t *testing.T) {
	tr := subagent.NewTree()
	err := tr.Add(subagent.Node{ID: "root", Agent: "root", State: subagent.NodeRunning})
	require.NoError(t, err)
	err = tr.Add(subagent.Node{ID: "child", Agent: "worker", Parent: "root", State: subagent.NodeIdle})
	require.NoError(t, err)
	m := New(tr)
	m.snapshot = tr.Snapshot()

	view := m.View()
	assert.Contains(t, view, "▶ root (running)")
	assert.Contains(t, view, "    worker (idle)")
}

func TestUpdateAppliesSnapshotsAndIgnoresStaleChannels(t *testing.T) {
	ch := make(chan subagent.Snapshot, 1)
	stale := make(chan subagent.Snapshot, 1)
	m := &Model{ch: ch}
	snap := subagent.Snapshot{Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "root", Agent: "root"}}}}

	_, cmd := m.Update(snapshotMsg{snapshot: snap, ch: stale})
	assert.Nil(t, cmd)
	assert.Empty(t, m.snapshot.Nodes)

	_, cmd = m.Update(snapshotMsg{snapshot: snap, ch: ch})
	assert.NotNil(t, cmd)
	require.Len(t, m.snapshot.Nodes, 1)
}

func TestUpdateRecordsWidth(t *testing.T) {
	m := New(nil)
	_, cmd := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	assert.Nil(t, cmd)
	assert.Equal(t, 80, m.width)
}
