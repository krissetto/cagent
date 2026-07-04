// Package subagentindex keeps a process-wide map of live subagent node ids to
// their display and agent names, fed from SubagentTreeEvent snapshots. Tool
// renderers use it to attribute a call by name while it is still running (the
// tool result, which carries the authoritative attribution, does not exist
// yet). It mirrors the styles agent-color registry pattern: a small global
// updated by the event stream and read from render paths.
package subagentindex

import (
	"sync"

	"github.com/docker/docker-agent/pkg/subagent"
)

var (
	mu    sync.RWMutex
	names = map[subagent.NodeID]string{}
)

// Update replaces the entries for every node present in the snapshot. Entries
// of finished nodes are kept: transcripts keep referring to them.
func Update(snapshot subagent.Snapshot) {
	mu.Lock()
	defer mu.Unlock()
	var walk func(nodes []subagent.NodeSnapshot)
	walk = func(nodes []subagent.NodeSnapshot) {
		for i := range nodes {
			names[nodes[i].Node.ID] = nodes[i].Node.DisplayName()
			walk(nodes[i].Children)
		}
	}
	walk(snapshot.Nodes)
}

// Name returns the display name recorded for a node id.
func Name(id subagent.NodeID) (string, bool) {
	mu.RLock()
	defer mu.RUnlock()
	name, ok := names[id]
	return name, ok
}
